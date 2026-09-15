package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"
)

func openTestLayerStore(t *testing.T) *LayerStore {
	t.Helper()
	s, err := OpenLayerStore(context.Background(), filepath.Join(t.TempDir(), "layers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func layerTestOptions() LayerOptions {
	return LayerOptions{ProjectID: "project-a", UserID: "user-a", OrganizationID: "org-a", SessionID: "session-a"}
}

func layerTestInput(kind Kind, scope Scope) LayerRecordInput {
	input := LayerRecordInput{Kind: kind, Scope: scope, Content: "Completed atlas build successfully", Source: "task:build", Revision: "abc123"}
	if kind == KindProcedural {
		input.Content = "Reusable atlas build procedure"
		input.Steps = []string{"Run make build", "Run make test"}
	}
	if kind == KindOrganizational {
		input.Content = "Curated atlas standard: tests accompany behavioral changes"
	}
	input.Publish = scope == ScopeOrganization
	return input
}

func mustLayerRecord(t *testing.T, s *LayerStore, ctx context.Context, o LayerOptions, input LayerRecordInput) LayerRecord {
	t.Helper()
	r, err := s.Record(ctx, o, input)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustLayerRecall(t *testing.T, s *LayerStore, ctx context.Context, o LayerOptions, q LayerQuery) []LayerRecord {
	t.Helper()
	records, err := s.Recall(ctx, o, q)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// BL-008 / AC-1: all four kinds survive reopen with exact provenance and FTS.
func TestLayerKindsReopen(t *testing.T) {
	ctx := storage.WithTenant(context.Background(), "tenant-a")
	path := filepath.Join(t.TempDir(), "layers.db")
	s, err := OpenLayerStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	o := layerTestOptions()
	o.SemanticEnabled = true
	expiry := time.Now().Add(time.Hour).UTC()
	var want []LayerRecord
	for _, kind := range []Kind{KindEpisodic, KindProcedural, KindSemantic, KindOrganizational} {
		scope := ScopeProject
		if kind == KindOrganizational {
			scope = ScopeOrganization
		}
		input := layerTestInput(kind, scope)
		input.ExpiresAt = &expiry
		r := mustLayerRecord(t, s, ctx, o, input)
		if r.Provenance.SessionID != o.SessionID || r.Provenance.Source != input.Source || r.Provenance.Revision != input.Revision ||
			r.Provenance.CreatedAt.IsZero() || !r.Provenance.CreatedAt.Equal(r.Provenance.UpdatedAt) || r.Provenance.Invalidated {
			t.Fatalf("missing provenance: %+v", r)
		}
		want = append(want, r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenLayerStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range want {
		got := mustLayerRecall(t, s, ctx, o, LayerQuery{Scope: r.Scope, Kind: r.Kind, Query: "atlas"})
		if len(got) != 1 || !reflect.DeepEqual(got[0], r) {
			t.Fatalf("reopened %s: got %+v, want %+v", r.Kind, got, r)
		}
		data, err := json.Marshal(got[0])
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["provenance"] == nil || fields["tenant_id"] != "tenant-a" || fields["kind"] != string(r.Kind) {
			t.Fatalf("JSON contract: %s", data)
		}
	}
	steps := mustLayerRecall(t, s, ctx, o, LayerQuery{Scope: ScopeProject, Query: "make test"})
	if len(steps) != 1 || steps[0].Kind != KindProcedural {
		t.Fatalf("procedural steps not searchable: %+v", steps)
	}
	if got := mustLayerRecall(t, s, ctx, o, LayerQuery{Scope: ScopeProject, Query: "null"}); len(got) != 0 {
		t.Fatalf("empty steps indexed as null: %+v", got)
	}
}

func TestLayerRecallRankingAndBudgets(t *testing.T) {
	s := openTestLayerStore(t)
	ctx := context.Background()
	o := layerTestOptions()
	input := layerTestInput(KindEpisodic, ScopeProject)
	input.Content = "atlas <>&雪"
	best := mustLayerRecord(t, s, ctx, o, input)
	input.Content = "atlas " + strings.Repeat("unrelated detail ", 30)
	other := mustLayerRecord(t, s, ctx, o, input)
	query := LayerQuery{Scope: ScopeProject, Query: "atlas"}
	for range 3 {
		got := mustLayerRecall(t, s, ctx, o, query)
		if len(got) != 2 || got[0].ID != best.ID || got[1].ID != other.ID {
			t.Fatalf("FTS relevance order: %+v", got)
		}
	}
	encoded, err := json.Marshal([]LayerRecord{best})
	if err != nil {
		t.Fatal(err)
	}
	query.MaxBytes = len(encoded)
	got := mustLayerRecall(t, s, ctx, o, query)
	if len(got) != 1 || got[0].ID != best.ID {
		t.Fatalf("exact JSON byte budget: %+v", got)
	}
	actual, _ := json.Marshal(got)
	if len(actual) != query.MaxBytes {
		t.Fatalf("budget includes escaping and metadata: got %d want %d", len(actual), query.MaxBytes)
	}
	query.MaxBytes--
	if got := mustLayerRecall(t, s, ctx, o, query); len(got) != 0 {
		t.Fatalf("record should not fit: %+v", got)
	}
	query.MaxBytes, query.Limit = 0, 1
	if got := mustLayerRecall(t, s, ctx, o, query); len(got) != 1 {
		t.Fatalf("record limit: %+v", got)
	}
	o.Budget = RetrievalBudget{MaxRecords: 1, MaxBytes: len(encoded)}
	query.MaxBytes, query.Limit = MaxLayerRecallBytes*2, MaxLayerRecords*2
	if got := mustLayerRecall(t, s, ctx, o, query); len(got) != 1 || got[0].ID != best.ID {
		t.Fatalf("query must not enlarge host budget: %+v", got)
	}
	for _, q := range []string{`" OR *`, `'); DROP TABLE layer_memories; --`, "***", ""} {
		if _, err := s.Recall(ctx, o, LayerQuery{Scope: ScopeProject, Query: q}); err != nil {
			t.Fatalf("literal query %q: %v", q, err)
		}
	}
	newest := mustLayerRecall(t, s, ctx, layerTestOptions(), LayerQuery{Scope: ScopeProject})
	if len(newest) != 2 || newest[0].ID != other.ID {
		t.Fatalf("newest-first listing: %+v", newest)
	}
	for range 12 {
		mustLayerRecord(t, s, ctx, layerTestOptions(), layerTestInput(KindEpisodic, ScopeProject))
	}
	if got := mustLayerRecall(t, s, ctx, layerTestOptions(), LayerQuery{Scope: ScopeProject}); len(got) != 10 {
		t.Fatalf("default record limit: %d", len(got))
	}
}

// BL-008 / AC-2: negative read AND mutation tests cover the tenant/project
// cross-product, each other identity, and identical IDs in different scope kinds.
func TestLayerScopeIsolation(t *testing.T) {
	s := openTestLayerStore(t)
	ctxA := storage.WithTenant(context.Background(), "tenant-a")
	ctxB := storage.WithTenant(context.Background(), "tenant-b")
	o := layerTestOptions()
	o.ProjectID, o.UserID, o.OrganizationID = "same-id", "same-id", "same-id"
	for _, scope := range []Scope{ScopeTenant, ScopeProject, ScopeUser, ScopeOrganization} {
		t.Run(string(scope), func(t *testing.T) {
			owner := mustLayerRecord(t, s, ctxA, o, layerTestInput(KindEpisodic, scope))
			foreign := o
			foreign.ProjectID, foreign.UserID, foreign.OrganizationID = "foreign", "foreign", "foreign"
			if scope != ScopeTenant {
				mustLayerRecord(t, s, ctxA, foreign, layerTestInput(KindEpisodic, scope))
			}
			mustLayerRecord(t, s, ctxB, o, layerTestInput(KindEpisodic, scope))
			mustLayerRecord(t, s, ctxB, foreign, layerTestInput(KindEpisodic, scope))
			for _, search := range []string{"", "atlas"} {
				got := mustLayerRecall(t, s, ctxA, o, LayerQuery{Scope: scope, Query: search})
				if len(got) != 1 || got[0].ID != owner.ID {
					t.Fatalf("owner partition leaked: %+v", got)
				}
			}
			for _, access := range []struct {
				ctx context.Context
				o   LayerOptions
			}{{ctxB, o}, {ctxB, foreign}, {ctxA, foreign}} {
				if scope == ScopeTenant && access.ctx == ctxA {
					continue // Explicit tenant scope intentionally shares across projects.
				}
				got := mustLayerRecall(t, s, access.ctx, access.o, LayerQuery{Scope: scope, Query: "atlas"})
				for _, r := range got {
					if r.ID == owner.ID {
						t.Fatalf("foreign scope read owner: %+v", r)
					}
				}
				if err := s.Forget(access.ctx, access.o, scope, owner.ID); !errors.Is(err, ErrLayerNotFound) {
					t.Fatalf("foreign forget: %v", err)
				}
			}
			for _, wrongScope := range []Scope{ScopeTenant, ScopeProject, ScopeUser, ScopeOrganization} {
				if wrongScope != scope {
					if err := s.Forget(ctxA, o, wrongScope, owner.ID); !errors.Is(err, ErrLayerNotFound) {
						t.Fatalf("cross-scope forget: %v", err)
					}
				}
			}
			otherProject := o
			otherProject.ProjectID = "other-project"
			got := mustLayerRecall(t, s, ctxA, otherProject, LayerQuery{Scope: scope})
			if scope == ScopeProject {
				if len(got) != 0 {
					t.Fatalf("cross-project read: %+v", got)
				}
			} else if len(got) != 1 || got[0].ID != owner.ID {
				t.Fatalf("explicit shared partition unavailable: %+v", got)
			}
			if err := s.Forget(ctxA, o, scope, owner.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, scope := range []Scope{ScopeProject, ScopeUser, ScopeOrganization, "", "all"} {
		if _, err := s.Recall(ctxA, LayerOptions{}, LayerQuery{Scope: scope}); err == nil {
			t.Fatalf("missing identity or invalid scope %q accepted", scope)
		}
	}
}

func TestLayerSemanticExpiryInvalidation(t *testing.T) {
	s := openTestLayerStore(t)
	ctx := context.Background()
	o := layerTestOptions()
	semantic := layerTestInput(KindSemantic, ScopeProject)
	if _, err := s.Record(ctx, o, semantic); err == nil {
		t.Fatal("semantic enabled by default")
	}
	enabled := o
	enabled.SemanticEnabled = true
	fact := mustLayerRecord(t, s, ctx, enabled, semantic)
	for _, query := range []string{"", "atlas"} {
		if got := mustLayerRecall(t, s, ctx, o, LayerQuery{Scope: ScopeProject, Query: query}); len(got) != 0 {
			t.Fatalf("semantic opt-out leaked: %+v", got)
		}
	}
	if _, err := s.Recall(ctx, o, LayerQuery{Scope: ScopeProject, Kind: KindSemantic}); err == nil {
		t.Fatal("explicit semantic query bypassed opt-out")
	}
	if err := s.Forget(ctx, o, ScopeProject, fact.ID); !errors.Is(err, ErrLayerNotFound) {
		t.Fatalf("disabled semantic mutation: %v", err)
	}
	expired := layerTestInput(KindEpisodic, ScopeProject)
	past := time.Now().Add(-time.Second)
	expired.ExpiresAt = &past
	old := mustLayerRecord(t, s, ctx, o, expired)
	active := mustLayerRecord(t, s, ctx, o, layerTestInput(KindEpisodic, ScopeProject))
	if err := s.Forget(ctx, o, ScopeProject, active.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old.ID, active.ID, "unknown"} {
		if err := s.Forget(ctx, o, ScopeProject, id); !errors.Is(err, ErrLayerNotFound) {
			t.Fatalf("inactive forget %s: %v", id, err)
		}
	}
	for _, query := range []string{"", "atlas"} {
		if got := mustLayerRecall(t, s, ctx, o, LayerQuery{Scope: ScopeProject, Query: query}); len(got) != 0 {
			t.Fatalf("expired/invalidated recall: %+v", got)
		}
	}
	var invalidated bool
	var created, updated int64
	var source, revision, session string
	if err := s.db.QueryRowContext(ctx, `SELECT invalidated, created_at, updated_at, source, revision, session_id FROM layer_memories WHERE id = ?`, active.ID).
		Scan(&invalidated, &created, &updated, &source, &revision, &session); err != nil {
		t.Fatal(err)
	}
	if !invalidated || updated < created || created != active.Provenance.CreatedAt.UnixNano() || source != active.Provenance.Source || revision != active.Provenance.Revision || session != o.SessionID {
		t.Fatal("invalidation did not retain provenance")
	}
}

func TestLayerRecordValidation(t *testing.T) {
	s := openTestLayerStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		edit func(*LayerOptions, *LayerRecordInput)
	}{
		{"unknown kind", func(o *LayerOptions, i *LayerRecordInput) { i.Kind = "instruction" }},
		{"unknown scope", func(o *LayerOptions, i *LayerRecordInput) { i.Scope = "global" }},
		{"no project", func(o *LayerOptions, i *LayerRecordInput) { o.ProjectID = "" }},
		{"no session", func(o *LayerOptions, i *LayerRecordInput) { o.SessionID = "" }},
		{"no source", func(o *LayerOptions, i *LayerRecordInput) { i.Source = " " }},
		{"no revision", func(o *LayerOptions, i *LayerRecordInput) { i.Revision = "" }},
		{"long revision", func(o *LayerOptions, i *LayerRecordInput) { i.Revision = strings.Repeat("a", 513) }},
		{"empty content", func(o *LayerOptions, i *LayerRecordInput) { i.Content = " " }},
		{"large content", func(o *LayerOptions, i *LayerRecordInput) { i.Content = strings.Repeat("x", MaxLayerContentBytes+1) }},
		{"invalid UTF8", func(o *LayerOptions, i *LayerRecordInput) { i.Content = string([]byte{0xff}) }},
		{"procedure without steps", func(o *LayerOptions, i *LayerRecordInput) { i.Kind = KindProcedural }},
		{"episode with steps", func(o *LayerOptions, i *LayerRecordInput) { i.Steps = []string{"do this"} }},
		{"empty step", func(o *LayerOptions, i *LayerRecordInput) { i.Kind = KindProcedural; i.Steps = []string{" "} }},
		{"combined size", func(o *LayerOptions, i *LayerRecordInput) {
			i.Kind = KindProcedural
			i.Steps = []string{strings.Repeat("x", MaxLayerContentBytes)}
		}},
		{"implicit publication", func(o *LayerOptions, i *LayerRecordInput) { i.Scope = ScopeOrganization }},
		{"missing org", func(o *LayerOptions, i *LayerRecordInput) {
			i.Scope = ScopeOrganization
			i.Publish = true
			o.OrganizationID = ""
		}},
		{"standard in project", func(o *LayerOptions, i *LayerRecordInput) { i.Kind = KindOrganizational }},
		{"publication in project", func(o *LayerOptions, i *LayerRecordInput) { i.Publish = true }},
		{"invalid expiry", func(o *LayerOptions, i *LayerRecordInput) { zero := time.Time{}; i.ExpiresAt = &zero }},
		{"negative budget", func(o *LayerOptions, i *LayerRecordInput) { o.Budget.MaxRecords = -1 }},
		{"excessive budget", func(o *LayerOptions, i *LayerRecordInput) { o.Budget.MaxBytes = MaxLayerRecallBytes + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, input := layerTestOptions(), layerTestInput(KindEpisodic, ScopeProject)
			tc.edit(&o, &input)
			if _, err := s.Record(ctx, o, input); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if got := mustLayerRecall(t, s, ctx, layerTestOptions(), LayerQuery{Scope: ScopeProject}); len(got) != 0 {
		t.Fatalf("validation wrote records: %+v", got)
	}
	input := layerTestInput(KindEpisodic, ScopeProject)
	input.Content = strings.Repeat("x", MaxLayerContentBytes)
	mustLayerRecord(t, s, ctx, layerTestOptions(), input)
}

func TestLayerSharedPathConcurrencyAndCancellation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := OpenLayerStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenLayerStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for n := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := []*LayerStore{a, b}[n%2]
			o := layerTestOptions()
			o.ProjectID = fmt.Sprintf("project-%d", n)
			ctx := storage.WithTenant(ctx, fmt.Sprintf("tenant-%d", n%2))
			for range 8 {
				r, err := s.Record(ctx, o, layerTestInput(KindEpisodic, ScopeProject))
				if err != nil {
					t.Error(err)
					return
				}
				got, err := s.Recall(ctx, o, LayerQuery{Scope: ScopeProject, Query: "atlas"})
				if err != nil || len(got) != 1 || got[0].ID != r.ID {
					t.Errorf("concurrent isolated recall: %+v, %v", got, err)
					return
				}
				if err := s.Forget(ctx, o, ScopeProject, r.ID); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if a.db.Stats().MaxOpenConnections != 1 || b.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("connection pool not bounded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	o := layerTestOptions()
	if _, err := a.Record(canceled, o, layerTestInput(KindEpisodic, ScopeProject)); !errors.Is(err, context.Canceled) {
		t.Fatalf("record cancellation: %v", err)
	}
	if _, err := a.Recall(canceled, o, LayerQuery{Scope: ScopeProject}); !errors.Is(err, context.Canceled) {
		t.Fatalf("recall cancellation: %v", err)
	}
	if err := a.Forget(canceled, o, ScopeProject, "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("forget cancellation: %v", err)
	}
	if opened, err := OpenLayerStore(canceled, path); !errors.Is(err, context.Canceled) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("open cancellation: %v", err)
	}
}

func layerToolMap(t *testing.T, s *LayerStore, o LayerOptions) map[string]*tool.Definition {
	t.Helper()
	definitions, err := LayerTools(s, o)
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]*tool.Definition)
	for _, def := range definitions {
		if tools[def.Name] != nil {
			t.Fatalf("duplicate tool %s", def.Name)
		}
		tools[def.Name] = def
	}
	if len(tools) != 3 || tools["memory_remember"] == nil || tools["memory_recall"] == nil || tools["memory_forget"] == nil {
		t.Fatalf("tool names: %+v", tools)
	}
	return tools
}

func rememberLayerArgs() map[string]any {
	return map[string]any{"kind": "episodic", "scope": "project", "content": "atlas task completed", "source": "task:1", "revision": "r1"}
}

// BL-008 / AC-3: handlers enforce validation without relying on schema checks.
func TestLayerToolsValidationAndBoundOptions(t *testing.T) {
	s := openTestLayerStore(t)
	ctx := storage.WithTenant(context.Background(), "tenant-a")
	o := layerTestOptions()
	o.Budget.MaxRecords = 1
	tools := layerToolMap(t, s, o)
	// Mutating the original value or advertised schema must not affect policy.
	o.ProjectID, o.OrganizationID, o.SessionID = "project-b", "org-b", "session-b"
	o.SemanticEnabled, o.Budget.MaxRecords = true, 100
	tools["memory_recall"].Parameters["properties"].(map[string]any)["limit"].(map[string]any)["maximum"] = 100
	value, err := tools["memory_remember"].Handler(ctx, rememberLayerArgs())
	if err != nil {
		t.Fatal(err)
	}
	r := value.(LayerRecord)
	if r.ProjectID != "project-a" || r.Provenance.SessionID != "session-a" {
		t.Fatalf("options were not bound: %+v", r)
	}
	for _, override := range []string{"project_id", "tenant_id", "user_id", "organization_id", "session_id", "semantic_enabled", "budget"} {
		for _, name := range []string{"memory_remember", "memory_recall", "memory_forget"} {
			args := rememberLayerArgs()
			if name == "memory_recall" {
				args = map[string]any{"scope": "project"}
			} else if name == "memory_forget" {
				args = map[string]any{"scope": "project", "id": r.ID}
			}
			args[override] = "foreign"
			if _, err := tools[name].Handler(ctx, args); err == nil {
				t.Fatalf("%s accepted identity/config override %s", name, override)
			}
		}
	}
	for _, edit := range []func(map[string]any){
		func(a map[string]any) { a["kind"] = "semantic" },
		func(a map[string]any) { a["kind"] = "bogus" },
		func(a map[string]any) { a["scope"] = "bogus" },
		func(a map[string]any) { a["scope"] = "organization" },
		func(a map[string]any) { a["kind"] = "procedural" },
		func(a map[string]any) { a["source"] = 10 },
		func(a map[string]any) { a["publish"] = "true" },
		func(a map[string]any) { a["content"] = nil },
		func(a map[string]any) { a["content"] = strings.Repeat("a", MaxLayerContentBytes+1) },
		func(a map[string]any) { a["expires_at"] = "not-a-time" },
		func(a map[string]any) { delete(a, "revision") },
	} {
		args := rememberLayerArgs()
		edit(args)
		if _, err := tools["memory_remember"].Handler(ctx, args); err == nil {
			t.Fatalf("invalid remember accepted: %+v", args)
		}
	}
	for _, args := range []map[string]any{
		{"scope": "project", "limit": 1.5}, {"scope": "project", "limit": -1},
		{"scope": "project", "max_bytes": 1}, {"scope": "project", "query": false},
		{"scope": "project", "kind": "semantic"}, {"scope": "project", "query": strings.Repeat("x", 1025)},
		{"scope": "all"}, {},
	} {
		if _, err := tools["memory_recall"].Handler(ctx, args); err == nil {
			t.Fatalf("invalid recall accepted: %+v", args)
		}
	}
	if _, err := tools["memory_remember"].Handler(ctx, rememberLayerArgs()); err != nil {
		t.Fatal(err)
	}
	got, err := tools["memory_recall"].Handler(ctx, map[string]any{"scope": "project", "limit": 100})
	if err != nil || len(got.([]LayerRecord)) != 1 {
		t.Fatalf("immutable budget: %+v, %v", got, err)
	}
	for _, access := range []struct {
		ctx   context.Context
		tools map[string]*tool.Definition
	}{{storage.WithTenant(ctx, "tenant-b"), tools}, {ctx, layerToolMap(t, s, o)}} {
		got, err := access.tools["memory_recall"].Handler(access.ctx, map[string]any{"scope": "project", "query": "atlas"})
		if err != nil || len(got.([]LayerRecord)) != 0 {
			t.Fatalf("foreign tool recall: %+v, %v", got, err)
		}
		if _, err := access.tools["memory_forget"].Handler(access.ctx, map[string]any{"scope": "project", "id": r.ID}); !errors.Is(err, ErrLayerNotFound) {
			t.Fatalf("foreign tool forget: %v", err)
		}
	}
	if _, err := tools["memory_forget"].Handler(ctx, map[string]any{"scope": "project", "id": r.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools["memory_forget"].Handler(ctx, map[string]any{"scope": "project", "id": r.ID}); !errors.Is(err, ErrLayerNotFound) {
		t.Fatalf("repeat tool forget: %v", err)
	}
	if _, err := LayerTools(nil, LayerOptions{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := LayerTools(s, LayerOptions{Budget: RetrievalBudget{MaxRecords: MaxLayerRecords + 1}}); err == nil {
		t.Fatal("invalid factory budget accepted")
	}
}

func TestLayerToolsExplicitPublicationAndKinds(t *testing.T) {
	s := openTestLayerStore(t)
	ctx := context.Background()
	o := layerTestOptions()
	o.SemanticEnabled = true
	tools := layerToolMap(t, s, o)
	for _, kind := range []Kind{KindEpisodic, KindProcedural, KindSemantic, KindOrganizational} {
		args := rememberLayerArgs()
		args["kind"] = string(kind)
		if kind == KindProcedural {
			args["steps"] = []any{"make build", "make test"}
		}
		if kind == KindOrganizational {
			args["scope"], args["publish"] = "organization", true
		}
		args["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		value, err := tools["memory_remember"].Handler(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		r := value.(LayerRecord)
		got, err := tools["memory_recall"].Handler(ctx, map[string]any{"scope": args["scope"], "kind": string(kind)})
		if err != nil || len(got.([]LayerRecord)) != 1 || got.([]LayerRecord)[0].ID != r.ID {
			t.Fatalf("tool kind %s recall: %+v, %v", kind, got, err)
		}
	}
	o.OrganizationID = ""
	noOrg := layerToolMap(t, s, o)
	args := rememberLayerArgs()
	args["scope"], args["kind"], args["publish"] = "organization", "organizational", true
	if _, err := noOrg["memory_remember"].Handler(ctx, args); err == nil {
		t.Fatal("explicit publish without configured org accepted")
	}
	if _, err := noOrg["memory_recall"].Handler(ctx, map[string]any{"scope": "organization"}); err == nil {
		t.Fatal("organization recall without configured org accepted")
	}
}
