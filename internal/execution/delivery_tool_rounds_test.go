package execution

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
)

func TestOperationJournalCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before tool runs", "after effect", "before result is saved", "after checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			clock := newControlledClock()
			path := filepath.Join(t.TempDir(), "delivery.db")
			store := openQueueTestStore(t, path, clock)
			admission := testAdmission("tenant", "repo", "delivery", "admission")
			if _, err := store.AdmitRunnable(ctx, admission); err != nil {
				t.Fatal(err)
			}
			old, err := store.Claim(ctx, "old", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			op := Operation{ID: "first:call", EffectKey: "first:call", Kind: "shell", RoleID: "worker", ReplayClass: ReplayUnknown, InputFingerprint: "input", ArgumentsFingerprint: "arguments"}
			if _, err := store.PrepareOperation(ctx, old, op); err != nil {
				t.Fatal(err)
			}
			effects := 0
			if boundary != "before tool runs" {
				if _, err := store.BeginOperation(ctx, old, op.ID); err != nil {
					t.Fatal(err)
				}
				effects++ // the destination performed the effect
			}
			if boundary == "before result is saved" {
				if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_receipt BEFORE UPDATE OF status ON delivery_operations WHEN NEW.status = 'observed' BEGIN SELECT RAISE(ABORT, 'injected crash'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.ObserveOperation(ctx, old, op.ID, json.RawMessage(`"effect"`), "output", ""); err == nil {
					t.Fatal("injected receipt failure was ignored")
				}
			}
			if boundary == "after checkpoint" {
				if _, err := store.ObserveOperation(ctx, old, op.ID, json.RawMessage(`"effect"`), "output", ""); err != nil {
					t.Fatal(err)
				}
				messages := []model.Message{
					{Role: model.RoleUser, Content: "task"},
					{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call", Name: "shell", Arguments: `{}`}}, ProviderState: []json.RawMessage{json.RawMessage(`{"id":"state"}`)}},
					{Role: model.RoleTool, ToolCallID: "call", Name: "shell", Content: `"effect"`},
				}
				mismatch := append([]model.Message(nil), messages...)
				mismatch[2].Content = `"unobserved"`
				if err := store.CheckpointToolRound(ctx, old, ToolRoundRecord{InvocationID: "first", RoleID: "worker", Input: "task", Model: "fixture", Number: 1, Messages: mismatch, EffectfulCalls: []string{"call"}}); !errors.Is(err, ErrEffectNeedsReconciliation) {
					t.Fatalf("invented tool result became a receipt: %v", err)
				}
				if err := store.CheckpointToolRound(ctx, old, ToolRoundRecord{InvocationID: "first", RoleID: "worker", Input: "task", Model: "fixture", Number: 1, Messages: messages, EffectfulCalls: []string{"call"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Minute + time.Second)
			store = openQueueTestStore(t, path, clock)
			replacement, err := store.Claim(ctx, "replacement", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Operation(ctx, admission.Scope, admission.DeliveryID, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			covered, err := store.CoversOperation(ctx, replacement, loaded)
			if err != nil {
				t.Fatal(err)
			}
			round, err := store.ResumeToolRound(ctx, replacement, "worker", "", "task", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "after checkpoint" {
				if !covered || len(round) != 3 || effects != 1 {
					t.Fatalf("durable exchange: covered=%t round=%+v effects=%d", covered, round, effects)
				}
				if _, ok := round[1].ProviderState.([]json.RawMessage); !ok {
					t.Fatalf("provider state decoded as %T", round[1].ProviderState)
				}
				if _, err := store.ResumeToolRound(ctx, replacement, "worker", "", "changed task", "fixture"); !errors.Is(err, ErrEffectNeedsReconciliation) {
					t.Fatalf("changed input resumed recorded exchange: %v", err)
				}
				if err := store.CheckpointToolRound(ctx, old, ToolRoundRecord{InvocationID: "old", RoleID: "worker", Input: "task", Model: "fixture", Number: 1, Messages: round}); !errors.Is(err, ErrStaleLease) {
					t.Fatalf("stale worker checkpointed: %v", err)
				}
				return
			}
			if covered || len(round) != 0 {
				t.Fatalf("uncheckpointed exchange was resumed: covered=%t round=%+v", covered, round)
			}
			if boundary == "before tool runs" {
				if effects != 0 || loaded.Status != OperationPrepared {
					t.Fatalf("pre-effect crash: op=%+v effects=%d", loaded, effects)
				}
				if _, err := store.PrepareOperation(ctx, replacement, op); err != nil {
					t.Fatalf("prepared intent did not survive: %v", err)
				}
			} else {
				if effects != 1 || loaded.Status != OperationRunning {
					t.Fatalf("ambiguous effect: op=%+v effects=%d", loaded, effects)
				}
				if _, err := store.PrepareOperation(ctx, replacement, op); !errors.Is(err, ErrEffectNeedsReconciliation) {
					t.Fatalf("running effect was replayed: %v", err)
				}
				if _, err := store.ReconcileOperation(ctx, replacement, op.ID, "model says success", "output", json.RawMessage(`"effect"`)); !errors.Is(err, ErrEffectNeedsReconciliation) {
					t.Fatalf("ambiguous effect became successful: %v", err)
				}
			}
		})
	}
}
