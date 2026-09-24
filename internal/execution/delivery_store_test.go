package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestDeliveryMigrationIsIdempotentAndChecksummed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestDeliveryStore(t, path)
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM delivery_schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != deliverySchemaVersion {
		t.Fatalf("migration marker count = %d, want %d", count, deliverySchemaVersion)
	}
	for _, migration := range deliveryMigrations {
		var checksum string
		if err := store.db.QueryRow(`SELECT checksum FROM delivery_schema_migrations WHERE version = ?`, migration.version).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		if checksum != migration.checksum {
			t.Fatalf("migration %d checksum = %q, want %q", migration.version, checksum, migration.checksum)
		}
	}
	if _, err := store.db.Exec(`UPDATE delivery_schema_migrations SET checksum = 'changed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDeliveryStore(ctx, path); !errors.Is(err, ErrIncompatibleDeliverySchema) {
		t.Fatalf("OpenDeliveryStore() checksum error = %v", err)
	}
}

func TestDeliveryMigrationUpgradesV1AndBackfillsMutationFingerprints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE delivery_schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL); `+deliverySchemaV1+`; INSERT INTO delivery_schema_migrations (version, checksum) VALUES (1, ?)`, deliveryMigrations[0].checksum); err != nil {
		t.Fatal(err)
	}
	transitionPayload := `{"from":"admitted","to":"queued","version":2}`
	if _, err := db.Exec(`INSERT INTO delivery_events (tenant_id, repository_id, delivery_id, sequence, event_id, idempotency_key, event_type, occurred_at, payload) VALUES ('tenant', 'repo', 'delivery', 1, 'transition', 'transition-key', ?, '2026-09-24T12:01:00Z', ?)`, DeliveryEventTransitioned, transitionPayload); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestDeliveryStore(t, path)
	var columnCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('delivery_events') WHERE name = 'request_fingerprint'`).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 1 {
		t.Fatalf("request_fingerprint columns = %d, want 1", columnCount)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(version) FROM delivery_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != deliverySchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, deliverySchemaVersion)
	}
	expectedFingerprint, err := deliveryMutationFingerprint(DeliveryEventTransitioned, 1, transitionedPayload{To: DeliveryQueued})
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err := store.db.QueryRow(`SELECT request_fingerprint FROM delivery_events WHERE event_id = 'transition'`).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if fingerprint != expectedFingerprint {
		t.Fatalf("backfilled fingerprint = %q, want %q", fingerprint, expectedFingerprint)
	}
}

func TestDeliveryStoreRefusesNewerSchemaBeforeMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	if _, err := store.db.Exec(`INSERT INTO delivery_schema_migrations (version, checksum) VALUES (?, 'future')`, deliverySchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDeliveryStore(ctx, path); !errors.Is(err, ErrUnsupportedDeliverySchema) {
		t.Fatalf("OpenDeliveryStore() newer schema error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM delivery_schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != deliverySchemaVersion+1 {
		t.Fatalf("migration marker count = %d, want %d", count, deliverySchemaVersion+1)
	}
}

func TestDeliveryAdmissionIsAtomicAndDuplicateDeterministic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	first, err := store.Admit(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	retry := admission
	retry.Goal.CreatedAt = admission.Goal.CreatedAt.Add(time.Hour)
	retry.Event.OccurredAt = admission.Event.OccurredAt.Add(time.Hour)
	second, err := store.Admit(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("duplicate admission changed result:\nfirst: %#v\nsecond: %#v", first, second)
	}
	conflict := admission
	conflict.Goal.Statement = "different goal"
	if _, err := store.Admit(ctx, conflict); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	conflict = admission
	conflict.Event.IdempotencyKey = "different-event-key"
	if _, err := store.Admit(ctx, conflict); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("conflicting admission event key error = %v", err)
	}
	conflict = admission
	conflict.AdmissionKey = "different-key"
	if _, err := store.Admit(ctx, conflict); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("duplicate delivery ID error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestDeliveryStore(t, path)
	loaded, err := store.Load(ctx, admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 1 || len(events) != 1 || events[0].Type != DeliveryEventAdmitted {
		t.Fatalf("persisted admission = %#v, events = %#v", loaded, events)
	}
}

func TestDeliveryCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	otherStore := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	delivery, err := store.Admit(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	stores := []*DeliveryStore{store, otherStore}
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := stores[i].Transition(ctx, admission.Scope, admission.DeliveryID, delivery.Version, DeliveryQueued, EventIdentity{ID: DeliveryEventID("queue-" + string(rune('a'+i))), IdempotencyKey: DeliveryIdempotencyKey("queue-key-" + string(rune('a'+i))), OccurredAt: admission.Event.OccurredAt.Add(time.Duration(i+1) * time.Minute)})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, ErrStaleDeliveryVersion) {
			t.Fatalf("transition error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners = %d, want 1", winners)
	}
	loaded, err := store.Load(ctx, admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != DeliveryQueued || loaded.Version != 2 {
		t.Fatalf("delivery after CAS = %#v", loaded)
	}
}

func TestDeliveryConcurrentIdentityReuseReturnsConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	stores := []*DeliveryStore{openTestDeliveryStore(t, path), openTestDeliveryStore(t, path)}
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	delivery, err := stores[0].Admit(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}

	states := []DeliveryState{DeliveryQueued, DeliveryPaused}
	event := testEvent("shared-transition", 1, admission.Event.OccurredAt)
	results := make(chan error, len(stores))
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Transition(ctx, admission.Scope, admission.DeliveryID, delivery.Version, states[i], event)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrEventConflict):
			conflicts++
		default:
			t.Fatalf("transition error = %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent results = %d winners, %d conflicts; want 1 each", winners, conflicts)
	}
}

func TestDeliveryMutationsRetryAfterLaterEventAndRejectIdentityConflicts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *DeliveryStore, Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery)
	}{
		{
			name: "transition",
			setup: func(t *testing.T, store *DeliveryStore, admission Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery) {
				event := testEvent("target-transition", 1, admission.Event.OccurredAt)
				mutate := func(identity EventIdentity, changed bool) (Delivery, error) {
					next := DeliveryQueued
					if changed {
						next = DeliveryPaused
					}
					return store.Transition(context.Background(), admission.Scope, admission.DeliveryID, 1, next, identity)
				}
				later := func(delivery Delivery) Delivery {
					return transitionDelivery(t, store, admission.Scope, delivery, DeliveryRunning, "later-transition", 2)
				}
				return event, mutate, later
			},
		},
		{
			name: "revise goal",
			setup: func(t *testing.T, store *DeliveryStore, admission Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery) {
				event := testEvent("target-goal", 1, admission.Event.OccurredAt)
				mutate := func(identity EventIdentity, changed bool) (Delivery, error) {
					statement := "revised goal"
					if changed {
						statement = "different revised goal"
					}
					goal := Goal{Revision: 2, Statement: statement, Actor: "user", CreatedAt: event.OccurredAt}
					requirements := []Requirement{{ID: "requirement-2", Statement: "new requirement", Status: RequirementAccepted}}
					return store.ReviseGoal(context.Background(), admission.Scope, admission.DeliveryID, 1, goal, requirements, identity)
				}
				later := func(delivery Delivery) Delivery {
					return transitionDelivery(t, store, admission.Scope, delivery, DeliveryQueued, "later-goal", 2)
				}
				return event, mutate, later
			},
		},
		{
			name: "update requirement",
			setup: func(t *testing.T, store *DeliveryStore, admission Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery) {
				event := testEvent("target-requirement", 1, admission.Event.OccurredAt)
				mutate := func(identity EventIdentity, changed bool) (Delivery, error) {
					status := RequirementSatisfied
					if changed {
						status = RequirementAccepted
					}
					return store.UpdateRequirement(context.Background(), admission.Scope, admission.DeliveryID, 1, "requirement-1", status, nil, identity)
				}
				later := func(delivery Delivery) Delivery {
					return transitionDelivery(t, store, admission.Scope, delivery, DeliveryQueued, "later-requirement", 2)
				}
				return event, mutate, later
			},
		},
		{
			name: "request decision",
			setup: func(t *testing.T, store *DeliveryStore, admission Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery) {
				event := testEvent("target-decision", 1, admission.Event.OccurredAt)
				mutate := func(identity EventIdentity, changed bool) (Delivery, error) {
					question := "Choose storage"
					if changed {
						question = "Choose a different storage system"
					}
					decision := Decision{ID: "decision-1", Question: question, Options: []string{"sqlite", "postgres"}, RequestedAt: event.OccurredAt}
					return store.RequestDecision(context.Background(), admission.Scope, admission.DeliveryID, 1, decision, identity)
				}
				later := func(delivery Delivery) Delivery {
					return transitionDelivery(t, store, admission.Scope, delivery, DeliveryQueued, "later-decision", 2)
				}
				return event, mutate, later
			},
		},
		{
			name: "resolve decision",
			setup: func(t *testing.T, store *DeliveryStore, admission Admission) (EventIdentity, func(EventIdentity, bool) (Delivery, error), func(Delivery) Delivery) {
				decision := Decision{ID: "decision-1", Question: "Choose storage", Options: []string{"sqlite", "postgres"}}
				if _, err := store.RequestDecision(context.Background(), admission.Scope, admission.DeliveryID, 1, decision, testEvent("setup-decision", 1, admission.Event.OccurredAt)); err != nil {
					t.Fatal(err)
				}
				event := testEvent("target-resolution", 2, admission.Event.OccurredAt)
				mutate := func(identity EventIdentity, changed bool) (Delivery, error) {
					choice := "sqlite"
					if changed {
						choice = "postgres"
					}
					resolution := DecisionResolution{Actor: "user", Choice: choice, Rationale: "scope", ResolvedAt: event.OccurredAt}
					return store.ResolveDecision(context.Background(), admission.Scope, admission.DeliveryID, 2, decision.ID, resolution, identity)
				}
				later := func(delivery Delivery) Delivery {
					return transitionDelivery(t, store, admission.Scope, delivery, DeliveryQueued, "later-resolution", 3)
				}
				return event, mutate, later
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
			admission := testAdmission("tenant", "repo", "delivery", "admission")
			if _, err := store.Admit(context.Background(), admission); err != nil {
				t.Fatal(err)
			}
			event, mutate, later := test.setup(t, store, admission)
			committed, err := mutate(event, false)
			if err != nil {
				t.Fatal(err)
			}
			current := later(committed)
			if reflect.DeepEqual(current, committed) {
				t.Fatal("later mutation did not change aggregate")
			}
			retried, err := mutate(event, false)
			if err != nil {
				t.Fatalf("retry error = %v", err)
			}
			if !reflect.DeepEqual(retried, committed) {
				t.Fatalf("retry returned wrong snapshot:\ncommitted: %#v\nretried: %#v\ncurrent: %#v", committed, retried, current)
			}
			conflicts := []EventIdentity{
				{ID: event.ID, IdempotencyKey: "different-key", OccurredAt: event.OccurredAt},
				{ID: "different-id", IdempotencyKey: event.IdempotencyKey, OccurredAt: event.OccurredAt},
			}
			for _, conflict := range conflicts {
				if _, err := mutate(conflict, true); !errors.Is(err, ErrEventConflict) {
					t.Fatalf("identity conflict error = %v", err)
				}
			}
		})
	}
}

func TestDeliveryScopeIsolationAndGuessedIDs(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	inside := testAdmission("tenant-a", "repo", "same-id", "inside")
	outside := testAdmission("tenant-b", "repo", "same-id", "outside")
	for _, admission := range []Admission{inside, outside} {
		if _, err := store.Admit(ctx, admission); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.List(ctx, inside.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != inside.DeliveryID {
		t.Fatalf("scoped list = %#v", listed)
	}
	missingScope := DeliveryScope{TenantID: "tenant-c", RepositoryID: "repo"}
	_, missingErr := store.Load(ctx, missingScope, inside.DeliveryID)
	_, guessedErr := store.Load(ctx, outside.Scope, DeliveryID("unknown"))
	if !errors.Is(missingErr, ErrDeliveryNotFound) || !errors.Is(guessedErr, ErrDeliveryNotFound) || missingErr.Error() != guessedErr.Error() {
		t.Fatalf("cross-scope error = %v, absent error = %v", missingErr, guessedErr)
	}
	if _, err := store.Events(ctx, missingScope, inside.DeliveryID); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("cross-scope events error = %v", err)
	}
}

func TestDeliveryEventsReplayOrderAndIdempotency(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	delivery, err := store.Admit(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	delivery = transitionDelivery(t, store, admission.Scope, delivery, DeliveryQueued, "queued", 1)
	delivery = transitionDelivery(t, store, admission.Scope, delivery, DeliveryRunning, "running", 2)
	goal := Goal{Revision: 2, Statement: "deliver revised foundation", Actor: "user", CreatedAt: admission.Event.OccurredAt.Add(3 * time.Minute)}
	requirements := []Requirement{{ID: "requirement-2", Statement: "replay exactly", Status: RequirementAccepted, Checks: []AcceptanceCheck{{ID: "check-2", Statement: "compare projection"}}}}
	delivery, err = store.ReviseGoal(ctx, admission.Scope, delivery.ID, delivery.Version, goal, requirements, testEvent("goal", 3, admission.Event.OccurredAt))
	if err != nil {
		t.Fatal(err)
	}
	waiver := &RequirementWaiver{Actor: "user", Reason: "external system unavailable"}
	delivery, err = store.UpdateRequirement(ctx, admission.Scope, delivery.ID, delivery.Version, "requirement-2", RequirementWaived, waiver, testEvent("waive", 4, admission.Event.OccurredAt))
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{ID: "decision-1", Question: "Choose storage", Options: []string{"sqlite", "postgres"}, Recommendation: "sqlite", Consequences: "local durability", Reversible: true, BlockedDependencies: []string{"worker"}}
	delivery, err = store.RequestDecision(ctx, admission.Scope, delivery.ID, delivery.Version, decision, testEvent("decision", 5, admission.Event.OccurredAt))
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = store.ResolveDecision(ctx, admission.Scope, delivery.ID, delivery.Version, decision.ID, DecisionResolution{Actor: "user", Choice: "sqlite", Rationale: "foundation scope"}, testEvent("resolution", 6, admission.Event.OccurredAt))
	if err != nil {
		t.Fatal(err)
	}

	identity := testEvent("checkpoint", 7, admission.Event.OccurredAt)
	payload := json.RawMessage(`{"snapshot":"abc"}`)
	firstEvent, err := store.AppendEvent(ctx, admission.Scope, delivery.ID, "checkpoint", payload, identity)
	if err != nil {
		t.Fatal(err)
	}
	duplicateEvent, err := store.AppendEvent(ctx, admission.Scope, delivery.ID, "checkpoint", payload, identity)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstEvent, duplicateEvent) {
		t.Fatalf("idempotent events differ: %#v != %#v", firstEvent, duplicateEvent)
	}
	if _, err := store.AppendEvent(ctx, admission.Scope, delivery.ID, "checkpoint", json.RawMessage(`{"snapshot":"different"}`), identity); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("conflicting event error = %v", err)
	}
	otherKey := identity
	otherKey.IdempotencyKey = "different-event-key"
	if _, err := store.AppendEvent(ctx, admission.Scope, delivery.ID, "checkpoint", payload, otherKey); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("reused event ID error = %v", err)
	}

	events, err := store.Events(ctx, admission.Scope, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("event %d sequence = %d", i, event.Sequence)
		}
	}
	replayed, err := ReplayDelivery(events)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, admission.Scope, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, loaded) {
		t.Fatalf("replayed delivery differs:\nreplayed: %#v\nloaded: %#v", replayed, loaded)
	}
	broken := append([]DeliveryEvent(nil), events...)
	broken[1].Sequence++
	if _, err := ReplayDelivery(broken); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("out-of-order replay error = %v", err)
	}
}

func TestDeliveryRunnableAndWaitingEnumeration(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	states := []DeliveryState{DeliveryAdmitted, DeliveryQueued, DeliveryRunning, DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused, DeliveryReconciling, DeliveryReplanning, DeliveryCancelRequested, DeliverySucceeded, DeliveryFailed, DeliveryCancelled}
	for i, target := range states {
		admission := testAdmission("tenant", "repo", DeliveryID("delivery-"+string(rune('a'+i))), AdmissionKey("admission-"+string(rune('a'+i))))
		delivery, err := store.Admit(ctx, admission)
		if err != nil {
			t.Fatal(err)
		}
		delivery = moveDeliveryToState(t, store, admission.Scope, delivery, target, admission.Event.OccurredAt)
	}
	runnable, err := store.ListRunnable(ctx, DeliveryScope{TenantID: "tenant", RepositoryID: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.ListWaiting(ctx, DeliveryScope{TenantID: "tenant", RepositoryID: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	assertSummaryStates(t, runnable, []DeliveryState{DeliveryAdmitted, DeliveryQueued, DeliveryReconciling, DeliveryReplanning, DeliveryCancelRequested})
	assertSummaryStates(t, waiting, []DeliveryState{DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused})
}

func TestDeliveryTransitionLegality(t *testing.T) {
	for _, terminal := range []DeliveryState{DeliverySucceeded, DeliveryFailed, DeliveryCancelled} {
		delivery := Delivery{State: terminal}
		if err := delivery.Transition(DeliveryQueued); !errors.Is(err, ErrInvalidDeliveryTransition) {
			t.Errorf("terminal %s transition error = %v", terminal, err)
		}
	}
	delivery := Delivery{State: DeliveryAdmitted}
	if err := delivery.Transition(DeliverySucceeded); !errors.Is(err, ErrInvalidDeliveryTransition) {
		t.Fatalf("admitted to succeeded error = %v", err)
	}
	if err := delivery.Transition(DeliveryQueued); err != nil {
		t.Fatalf("admitted to queued: %v", err)
	}
}

func openTestDeliveryStore(t *testing.T, path string) *DeliveryStore {
	t.Helper()
	store, err := OpenDeliveryStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testAdmission(tenant TenantID, repository RepositoryID, deliveryID DeliveryID, key AdmissionKey) Admission {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	return Admission{
		Scope:           DeliveryScope{TenantID: tenant, RepositoryID: repository},
		DeliveryID:      deliveryID,
		AdmissionKey:    key,
		Goal:            Goal{Statement: "build durable delivery", Actor: "user", CreatedAt: at},
		Requirements:    []Requirement{{ID: "requirement-1", Statement: "persist before return", Status: RequirementAccepted, Checks: []AcceptanceCheck{{ID: "check-1", Statement: "reopen database"}}}},
		PolicyReference: "policy-v1",
		Event:           EventIdentity{ID: DeliveryEventID("event-" + key), IdempotencyKey: DeliveryIdempotencyKey("event-key-" + key), OccurredAt: at},
	}
}

func testEvent(name string, minute int, base time.Time) EventIdentity {
	return EventIdentity{ID: DeliveryEventID(name), IdempotencyKey: DeliveryIdempotencyKey(name + "-key"), OccurredAt: base.Add(time.Duration(minute) * time.Minute)}
}

func transitionDelivery(t *testing.T, store *DeliveryStore, scope DeliveryScope, delivery Delivery, state DeliveryState, name string, minute int) Delivery {
	t.Helper()
	next, err := store.Transition(context.Background(), scope, delivery.ID, delivery.Version, state, testEvent(name, minute, delivery.CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func moveDeliveryToState(t *testing.T, store *DeliveryStore, scope DeliveryScope, delivery Delivery, target DeliveryState, base time.Time) Delivery {
	t.Helper()
	var path []DeliveryState
	switch target {
	case DeliveryAdmitted:
		return delivery
	case DeliveryQueued:
		path = []DeliveryState{DeliveryQueued}
	case DeliveryRunning:
		path = []DeliveryState{DeliveryQueued, DeliveryRunning}
	case DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota:
		path = []DeliveryState{DeliveryQueued, DeliveryRunning, target}
	case DeliveryPaused, DeliveryReplanning, DeliveryReconciling, DeliveryCancelRequested, DeliveryFailed:
		path = []DeliveryState{DeliveryQueued, target}
	case DeliverySucceeded:
		path = []DeliveryState{DeliveryQueued, DeliveryRunning, target}
	case DeliveryCancelled:
		path = []DeliveryState{DeliveryCancelRequested, target}
	default:
		t.Fatalf("unsupported target state %s", target)
	}
	for i, state := range path {
		delivery = transitionDelivery(t, store, scope, delivery, state, string(delivery.ID)+"-transition-"+string(rune('a'+i)), i+1)
		delivery.UpdatedAt = base.Add(time.Duration(i+1) * time.Minute)
	}
	return delivery
}

func assertSummaryStates(t *testing.T, summaries []DeliverySummary, expected []DeliveryState) {
	t.Helper()
	counts := make(map[DeliveryState]int)
	for _, summary := range summaries {
		counts[summary.State]++
	}
	if len(summaries) != len(expected) {
		t.Fatalf("summary states = %#v, want %v", summaries, expected)
	}
	for _, state := range expected {
		if counts[state] != 1 {
			t.Fatalf("state %s count = %d in %#v", state, counts[state], summaries)
		}
	}
}
