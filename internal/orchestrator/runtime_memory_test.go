package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"
	storagesqlite "github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/verification"
)

func runtimeTestEnvironment(t *testing.T) (root, home string) {
	t.Helper()
	root, home = t.TempDir(), t.TempDir()
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHRONOS_CODE_DATA_HOME", home)
	return root, home
}

func runtimeTestConfig(root string) *config.Config {
	off := false
	return &config.Config{
		FileConfig: agent.FileConfig{Agents: []agent.AgentConfig{{
			ID: "coder", Name: "Coder",
			Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"},
		}}},
		Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &off},
		MCP:       config.MCPConfig{Discovery: &off},
	}
}

func TestRuntimeSnapshotIncludesWALAndNeverClobbers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "legacy.db"), filepath.Join(dir, "new", "sessions.db")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE evidence(value TEXT)", "INSERT INTO evidence VALUES ('committed-in-wal')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO evidence VALUES ('uncommitted')"); err != nil {
		t.Fatal(err)
	}
	if err := snapshotLegacyDatabase(ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	copyDB, err := sql.Open("sqlite", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	if err := copyDB.QueryRow("SELECT count(*) FROM evidence").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot count = %d, %v; want committed WAL row only", count, err)
	}
	if _, err := copyDB.Exec("INSERT INTO evidence VALUES ('destination-owned')"); err != nil {
		t.Fatal(err)
	}
	if err := snapshotLegacyDatabase(ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	if err := copyDB.QueryRow("SELECT count(*) FROM evidence").Scan(&count); err != nil || count != 2 {
		t.Fatalf("destination overwritten: count=%d err=%v", count, err)
	}
	if _, err := os.Stat(source + "-wal"); err != nil {
		t.Fatalf("original live WAL removed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM evidence").Scan(&count); err != nil || count != 1 {
		t.Fatalf("source modified: count=%d err=%v", count, err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(destination), ".sqlite-snapshot-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary snapshots leaked: %v", matches)
	}
}

func TestRuntimeSnapshotFailureRetainsOriginal(t *testing.T) {
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "legacy.db"), filepath.Join(dir, "new.db")
	original := []byte("not a SQLite database")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshotLegacyDatabase(context.Background(), source, destination); err == nil {
		t.Fatal("corrupt legacy data silently discarded")
	}
	data, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("original lost: %q %v", data, err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid destination published: %v", err)
	}
}

func TestRuntimeNewMigratesGraphTelemetryAndPreservesYAML(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	cfg.Memory.Enabled, cfg.Learning.Enabled = true, true
	legacyDir := filepath.Join(root, config.ConfigDirName)
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"graph.db", "memory.db"} {
		db, err := sql.Open("sqlite", filepath.Join(legacyDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE legacy_evidence(value TEXT)", "INSERT INTO legacy_evidence VALUES ('retained')"} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	yamlStore := memory.NewStore(filepath.Join(legacyDir, "memory"))
	if _, err := yamlStore.Add(memory.CategoryProject, "legacy YAML project note"); err != nil {
		t.Fatal(err)
	}
	orch := newLayerTestOrchestrator(t, cfg)
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.GraphDB, paths.TelemetryDB} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		var value string
		err = db.QueryRow("SELECT value FROM legacy_evidence").Scan(&value)
		_ = db.Close()
		if err != nil || value != "retained" {
			t.Fatalf("New lost legacy data in %s: %q %v", path, value, err)
		}
	}
	notes, err := orch.memory.List(memory.CategoryProject)
	if err != nil || len(notes) != 1 || notes[0].Content != "legacy YAML project note" {
		t.Fatalf("legacy YAML not rooted at workspace: %+v %v", notes, err)
	}
}

func TestRuntimeNilConfigAndExplicitSQLiteURI(t *testing.T) {
	root, home := runtimeTestEnvironment(t)
	t.Chdir(root)
	orch, err := New(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	if orch.runtimeMemory != nil || !strings.HasPrefix(orch.cfg.Workspace.GraphDB, home) {
		t.Fatal("nil config lost safe disabled memory/default path behavior")
	}
	for _, dsn := range []string{":memory:", "file:plan08-in-memory?mode=memory&cache=shared"} {
		cfg := runtimeTestConfig(root)
		cfg.Defaults = &agent.AgentConfig{Storage: agent.StorageConfig{DSN: dsn}}
		store, actual, err := OpenStorageForCLI(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		if actual != dsn {
			t.Fatalf("SQLite URI changed: %q -> %q", dsn, actual)
		}
	}
}

func TestRuntimeExplicitDefaultLocationDoesNotImportLegacy(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LegacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions.db", "graph.db"} {
		if err := os.WriteFile(filepath.Join(paths.LegacyDir, name), []byte("unreadable old database"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Even an explicit path spelled exactly like the new default is an override.
	cfg.Defaults = &agent.AgentConfig{Storage: agent.StorageConfig{DSN: paths.SessionsDB}}
	cfg.Workspace.GraphDB = paths.GraphDB
	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("explicit location incorrectly attempted legacy import: %v", err)
	}
	defer orch.Close()
}

func TestRuntimeStorageCLIAndNewResolveSameDefault(t *testing.T) {
	root, home := runtimeTestEnvironment(t)
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: external-worktree"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "nested")
	if err := os.MkdirAll(filepath.Join(root, config.ConfigDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	legacyPath := filepath.Join(root, config.ConfigDirName, "sessions.db")
	legacy, err := storagesqlite.New(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	ctx := context.Background()
	if err := legacy.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := legacy.CreateSession(ctx, &storage.Session{ID: "legacy-session", AgentID: "coder", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	embedded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	store, dsn, err := OpenStorageForCLI(embedded)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !strings.HasPrefix(dsn, filepath.Join(home, "projects")) {
		t.Fatalf("default remains project-local: %s", dsn)
	}
	if _, err := store.GetSession(ctx, "legacy-session"); err != nil {
		t.Fatalf("CLI lost legacy session: %v", err)
	}
	cfg := runtimeTestConfig(root)
	orch, err := New(ctx, cfg, "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	if _, err := orch.store.GetSession(ctx, "legacy-session"); err != nil {
		t.Fatalf("New and CLI disagree: %v", err)
	}
	if cfg.Workspace.GraphDB != "" {
		t.Fatal("New rewrote caller configuration")
	}
	if orch.cfg.Workspace.Root != root || !strings.HasPrefix(orch.cfg.Workspace.GraphDB, home) {
		t.Fatalf("unresolved runtime workspace: %+v", orch.cfg.Workspace)
	}
	// An explicitly supplied legacy path is used in place rather than migrated.
	cfg.Defaults = &agent.AgentConfig{Storage: agent.StorageConfig{DSN: ".chronos-code/sessions.db"}}
	explicit, explicitDSN, err := OpenStorageForCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.Close()
	if explicitDSN != legacyPath {
		t.Fatalf("explicit path redirected: %s", explicitDSN)
	}
}

func TestRuntimeNewOwnsSharedCustomAndAsyncResources(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	cfg.Memory.Enabled = true
	cfg.Learning.Enabled = true
	for _, id := range []string{"peer", "custom", "stateless"} {
		ac := cfg.Agents[0]
		ac.ID = id
		if id == "custom" {
			ac.Storage = agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(root, "custom.db")}
		} else if id == "stateless" {
			ac.Storage = agent.StorageConfig{Backend: "none"}
		}
		cfg.Agents = append(cfg.Agents, ac)
	}
	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	if orch.agents["coder"].Storage != orch.store || orch.agents["peer"].Storage != orch.store {
		t.Fatal("default agents did not borrow the one shared store")
	}
	custom := orch.agents["custom"].Storage
	if custom == nil || custom == orch.store || orch.agents["stateless"].Storage != nil {
		t.Fatal("custom or explicitly disabled storage overridden")
	}
	if len(orch.telemetryRecorders) != 4 || orch.runtimeMemory == nil {
		t.Fatal("runtime resources not retained for cleanup")
	}
	ctx := storage.WithSession(context.Background(), "telemetry-session")
	for _, recorder := range orch.telemetryRecorders {
		evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "test"}
		if err := recorder.Before(ctx, evt); err != nil {
			t.Fatal(err)
		}
		evt.Type = hooks.EventModelCallAfter
		evt.Output = &model.ChatResponse{Content: "done"}
		if err := recorder.After(ctx, evt); err != nil {
			t.Fatal(err)
		}
	}
	orch.actBuf.Enqueue(ctx, orch.graphStore, "example")
	if err := orch.Close(); err != nil {
		t.Fatal(err)
	}
	for _, recorder := range orch.telemetryRecorders {
		stats := recorder.Stats()
		if stats.Accepted != 2 || stats.Processed != stats.Accepted || stats.Errors != 0 {
			t.Fatalf("telemetry not drained before SQL close: %+v", stats)
		}
		if err := recorder.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore}); err != nil {
			t.Fatalf("optional closed telemetry aborted execution: %v", err)
		}
	}
	if _, err := custom.ListSessions(ctx, "", 10, 0); err == nil {
		t.Fatal("custom agent store leaked")
	}
	if orch.actBuf.Enqueue(ctx, orch.graphStore, "after-close") {
		t.Fatal("activation buffer still accepting work")
	}
	if _, err := orch.graphStore.Stats(ctx); err == nil {
		t.Fatal("graph store leaked")
	}
	if _, err := orch.runtimeMemory.store.Recall(ctx, orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject}); err == nil {
		t.Fatal("layer store leaked")
	}
}

func TestRuntimeCloseDeduplicatesOwnedStorage(t *testing.T) {
	shared, custom := &closeTrackingStorage{}, &closeTrackingStorage{}
	orch := &Orchestrator{store: shared, agents: map[string]*agent.Agent{
		"a": {Storage: shared}, "b": {Storage: custom}, "c": {Storage: custom},
	}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = orch.Close() })
	}
	wg.Wait()
	if shared.closeCount() != 1 || custom.closeCount() != 1 {
		t.Fatalf("close counts shared=%d custom=%d", shared.closeCount(), custom.closeCount())
	}
}

func TestRuntimeStartupFailureClosesWatchersAndStorage(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	on := true
	cfg.Workspace.IndexOnStart = &on
	cfg.Memory.Enabled = true
	cfg.Learning.Enabled = true
	cfg.Defaults = &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "postgres", DSN: "test-owned-store"}}
	shared := &closeTrackingStorage{Storage: storagememory.New()}
	previous := postgresOpener
	postgresOpener = func(string) (storage.Storage, error) { return shared, nil }
	defer func() { postgresOpener = previous }()
	if err := os.MkdirAll(filepath.Join(root, config.ConfigDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigDirName, "security.yaml"), []byte("invalid: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Use bounded reads."), 0o600); err != nil {
		t.Fatal(err)
	}
	countWorkers := func() int {
		var profile bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&profile, 2)
		return strings.Count(profile.String(), "internal/graph.(*Watcher).loop") +
			strings.Count(profile.String(), "internal/projectdocs.(*Watcher).loop") +
			strings.Count(profile.String(), "internal/learning.(*TelemetryRecorder).run")
	}
	before := countWorkers()
	if _, err := New(context.Background(), cfg, ""); err == nil || !strings.Contains(err.Error(), "configure security") {
		t.Fatalf("expected late startup failure, got %v", err)
	}
	if shared.closeCount() != 1 {
		t.Fatalf("startup shared storage close count=%d", shared.closeCount())
	}
	if after := countWorkers(); after != before {
		t.Fatalf("startup watcher/telemetry workers leaked: before=%d after=%d", before, after)
	}
}

func newLayerTestOrchestrator(t *testing.T, cfg *config.Config) *Orchestrator {
	t.Helper()
	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	orch.agents["coder"].Model = &executionTestProvider{name: "bounded final outcome", modelID: "gpt-4o-mini"}
	return orch
}

func layerTool(t *testing.T, orch *Orchestrator, ctx context.Context, name string, args map[string]any) any {
	t.Helper()
	def, ok := orch.agents["coder"].Tools.Get(name)
	if !ok {
		t.Fatalf("missing tool %s", name)
	}
	result, err := def.Handler(ctx, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	// Runtime compression may serialize tool results; normalize their JSON.
	if text, ok := result.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	return result
}

func TestRuntimeLayersCrossProjectScopesAndSessionProvenance(t *testing.T) {
	root, home := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	cfg.Memory = config.MemoryConfig{Enabled: true, OrganizationID: "org-one"}
	one := newLayerTestOrchestrator(t, cfg)
	secondCfg := runtimeTestConfig(t.TempDir())
	secondCfg.Memory = cfg.Memory
	two := newLayerTestOrchestrator(t, secondCfg)
	ctx := storage.WithSession(storage.WithTenant(context.Background(), "tenant-a"), "explicit-session")
	for _, scope := range []memory.Scope{memory.ScopeProject, memory.ScopeUser, memory.ScopeOrganization, memory.ScopeTenant} {
		kind := memory.KindProcedural
		args := map[string]any{"scope": scope, "kind": kind, "content": "release workflow", "steps": []string{"run tests", "publish artifact"}, "source": "user", "revision": "v1"}
		if scope == memory.ScopeOrganization {
			args["publish"] = true
		}
		layerTool(t, one, ctx, "memory_remember", args)
		records, err := two.runtimeMemory.store.Recall(ctx, two.runtimeMemory.options, memory.LayerQuery{Scope: scope, Query: "release"})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if scope == memory.ScopeProject {
			want = 0
		}
		if len(records) != want {
			t.Fatalf("cross-project %s records=%d want=%d", scope, len(records), want)
		}
		if len(records) > 0 && records[0].Provenance.SessionID != "explicit-session" {
			t.Fatalf("static startup session provenance: %+v", records[0])
		}
	}
	if _, err := os.Stat(filepath.Join(home, "memory.db")); err != nil {
		t.Fatalf("shared layer DB missing: %v", err)
	}
	layerTool(t, one, ctx, "memory_remember", map[string]any{"scope": "organization", "kind": "organizational", "content": "curated release standard", "publish": true, "source": "user:publish", "revision": "v2"})
	standards, err := two.runtimeMemory.store.Recall(ctx, two.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeOrganization, Kind: memory.KindOrganizational})
	if err != nil || len(standards) != 1 || !standards[0].Published {
		t.Fatalf("explicit organization standard not shared: %+v %v", standards, err)
	}
	otherTenant := storage.WithTenant(ctx, "tenant-b")
	records, err := two.runtimeMemory.store.Recall(otherTenant, two.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeOrganization})
	if err != nil || len(records) != 0 {
		t.Fatalf("tenant leaked: %+v %v", records, err)
	}
	def, _ := one.agents["coder"].Tools.Get("memory_remember")
	for _, args := range []map[string]any{
		{"scope": "organization", "kind": "organizational", "content": "unpublished standard", "source": "user", "revision": "v1"},
		{"scope": "project", "kind": "semantic", "content": "disabled fact", "source": "user", "revision": "v1"},
	} {
		if _, err := def.Handler(ctx, args); err == nil {
			t.Fatalf("unsafe/disabled write accepted: %+v", args)
		}
	}
	// The same registered handler uses the resumed context, not its first call.
	ctx = storage.WithSession(ctx, "resumed-session")
	layerTool(t, one, ctx, "memory_remember", map[string]any{"scope": "project", "kind": "episodic", "content": "resumed outcome", "source": "user", "revision": "v2"})
	records, err = one.runtimeMemory.store.Recall(ctx, one.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject, Query: "resumed"})
	if err != nil || len(records) != 1 || records[0].Provenance.SessionID != "resumed-session" {
		t.Fatalf("resumed provenance: %+v %v", records, err)
	}
	layerTool(t, one, ctx, "memory_forget", map[string]any{"scope": "project", "id": records[0].ID})
	records, err = one.runtimeMemory.store.Recall(ctx, one.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject, Query: "resumed"})
	if err != nil || len(records) != 0 {
		t.Fatalf("forget did not invalidate: %+v %v", records, err)
	}
}

func TestRuntimeConfiguredUserMemoryIsolation(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	cfg.Memory.Enabled = true
	cfg.Agents[0].UserID = "alice"
	alice := newLayerTestOrchestrator(t, cfg)
	otherCfg := runtimeTestConfig(t.TempDir())
	otherCfg.Memory.Enabled = true
	otherCfg.Agents[0].UserID = "bob"
	bob := newLayerTestOrchestrator(t, otherCfg)
	ctx := storage.WithSession(context.Background(), "user-session")
	layerTool(t, alice, ctx, "memory_remember", map[string]any{"scope": "user", "kind": "episodic", "content": "private preference", "source": "user", "revision": "v1"})
	for _, tc := range []struct {
		orch *Orchestrator
		want int
	}{{alice, 1}, {bob, 0}} {
		result := layerTool(t, tc.orch, ctx, "memory_recall", map[string]any{"scope": "user", "query": "private"})
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var records []memory.LayerRecord
		if err := json.Unmarshal(data, &records); err != nil || len(records) != tc.want {
			t.Fatalf("user recall %s: %s %v", tc.orch.agents["coder"].UserID, data, err)
		}
	}
}

func TestRuntimeExecuteRecordsBoundedEpisodesAndRecallsData(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	cfg.Memory = config.MemoryConfig{Enabled: true, OrganizationID: "org", ContextBudgetTokens: 3000, MaxRecords: 3}
	orch := newLayerTestOrchestrator(t, cfg)
	ctx := storage.WithTenant(context.Background(), "tenant")
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		request := ExecutionRequest{Message: "migration " + strings.Repeat("界", 500), SessionID: fmt.Sprintf("explicit-%d", mode), TaskID: fmt.Sprintf("task-%d", mode), Mode: mode}
		result, err := orch.Execute(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		var chunks int
		if mode == ExecutionStreaming {
			for response := range result.Stream {
				if response.Err != nil {
					t.Fatal(response.Err)
				}
				chunks++
			}
			if chunks == 0 {
				t.Fatal("observer consumed UI stream")
			}
		}
	}
	inspectionOptions := orch.runtimeMemory.options
	inspectionOptions.Budget.MaxBytes = memory.MaxLayerRecallBytes
	records, err := orch.runtimeMemory.store.Recall(ctx, inspectionOptions, memory.LayerQuery{Scope: memory.ScopeProject, Kind: memory.KindEpisodic, Query: "migration"})
	if err != nil || len(records) != 2 {
		t.Fatalf("Execute did not record episodes: %+v %v", records, err)
	}
	for _, record := range records {
		if len(record.Content) > 3200 || !utf8.ValidString(record.Content) || !strings.Contains(record.Content, "bounded final outcome") || !strings.HasPrefix(record.Provenance.SessionID, "explicit-") || record.Published {
			t.Fatalf("invalid automatic episode: %+v", record)
		}
	}
	org, err := orch.runtimeMemory.store.Recall(ctx, orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeOrganization})
	if err != nil || len(org) != 0 {
		t.Fatalf("episode auto-published: %+v %v", org, err)
	}
	queryCtx := context.WithValue(storage.WithSession(ctx, "query-session"), messageKey{}, "migration")
	pin := orch.runtimeMemory.recallPin(queryCtx)
	if !strings.HasPrefix(pin, layerDataHeader) || len(pin) > 3000 {
		t.Fatalf("unbounded/non-DATA recall: %q", pin)
	}
	if pin := orch.runtimeMemory.recallPin(context.WithValue(queryCtx, messageKey{}, "unrelatedneedle")); pin != "" {
		t.Fatalf("irrelevant layer pinned: %q", pin)
	}
	result, err := orch.Execute(ctx, ExecutionRequest{Message: "migration", SessionID: "query-session"})
	if err != nil || result.Response == nil {
		t.Fatalf("recall Execute: %+v %v", result, err)
	}
	provider := orch.agents["coder"].Model.(*executionTestProvider)
	if !strings.Contains(strings.Join(messageContents(provider.request(2).Messages), "\n"), layerDataHeader) {
		t.Fatal("layer DATA pins not integrated into model request")
	}
}

type runtimeToolProvider struct {
	mu       sync.Mutex
	requests []*model.ChatRequest
	call     int
}

func (p *runtimeToolProvider) Name() string  { return "runtime-tools" }
func (p *runtimeToolProvider) Model() string { return "gpt-4o-mini" }
func (p *runtimeToolProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	p.call++
	switch p.call {
	case 1:
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "remember-call", Name: "memory_remember", Arguments: `{"scope":"project","kind":"procedural","content":"release procedure","steps":["test","release"],"source":"user:release","revision":"v1"}`}}}, nil
	case 2:
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "recall-call", Name: "memory_recall", Arguments: `{"scope":"project","query":"release"}`}}}, nil
	default:
		return &model.ChatResponse{Content: "Remembered and recalled the release procedure.", StopReason: model.StopReasonEnd}, nil
	}
}

func (p *runtimeToolProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	response, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan *model.ChatResponse, 1)
	out <- response
	close(out)
	return out, nil
}

func TestRuntimeExecuteCallsRememberAndRecallTools(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			root, _ := runtimeTestEnvironment(t)
			cfg := runtimeTestConfig(root)
			cfg.Memory.Enabled = true
			orch := newLayerTestOrchestrator(t, cfg)
			provider := &runtimeToolProvider{}
			orch.agents["coder"].Model = provider
			result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "Save and look up the release procedure", SessionID: "tool-session", Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if result.Stream != nil {
				for response := range result.Stream {
					if response.Err != nil {
						t.Fatal(response.Err)
					}
				}
			}
			provider.mu.Lock()
			calls := provider.call
			last := provider.requests[len(provider.requests)-1]
			provider.mu.Unlock()
			if calls != 3 || !strings.Contains(strings.Join(messageContents(last.Messages), "\n"), "release procedure") {
				t.Fatalf("tool loop failed: calls=%d request=%+v", calls, last)
			}
			records, err := orch.runtimeMemory.store.Recall(context.Background(), orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject, Kind: memory.KindProcedural})
			if err != nil || len(records) != 1 || records[0].Provenance.SessionID != "tool-session" || len(records[0].Steps) != 2 {
				t.Fatalf("Execute tool provenance/persistence: %+v %v", records, err)
			}
		})
	}
}

func TestRuntimeSemanticOptInAndDelegateReusesMemory(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	on := true
	cfg.Memory = config.MemoryConfig{Enabled: true, SemanticEnabled: &on}
	orch := newLayerTestOrchestrator(t, cfg)
	ctx := storage.WithSession(context.Background(), "delegate-session")
	ctx = context.WithValue(ctx, messageKey{}, "compiler")
	layerTool(t, orch, ctx, "memory_remember", map[string]any{"scope": "user", "kind": "semantic", "content": "compiler fact", "source": "user", "revision": "v1"})
	svc, err := harness.NewSubAgentService(orch.agents["coder"])
	if err != nil {
		t.Fatal(err)
	}
	_, err = harness.NewInProcessRunner(svc).Run(ctx, harness.SubAgentSpec{Name: "memory-reader", SystemPrompt: "Read context data", ToolNames: []string{"memory_recall"}}, "compiler")
	if err != nil {
		t.Fatal(err)
	}
	provider := orch.agents["coder"].Model.(*executionTestProvider)
	if !strings.Contains(strings.Join(messageContents(provider.request(0).Messages), "\n"), "compiler fact") {
		t.Fatal("lightweight delegate lost shared memory pins")
	}
	if _, err := orch.runtimeMemory.store.Recall(ctx, orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeUser}); err != nil {
		t.Fatalf("delegate closed borrowed store: %v", err)
	}
}

func TestRuntimeStreamErrorsCancellationAndShutdown(t *testing.T) {
	for _, scenario := range []string{"error", "cancel", "close", "verification"} {
		t.Run(scenario, func(t *testing.T) {
			root, _ := runtimeTestEnvironment(t)
			cfg := runtimeTestConfig(root)
			cfg.Memory.Enabled = true
			orch := newLayerTestOrchestrator(t, cfg)
			ctx, cancel := context.WithCancel(storage.WithSession(context.Background(), "stream-session"))
			defer cancel()
			ctx = context.WithValue(ctx, taskIDKey{}, "stream-task")
			if scenario == "verification" {
				result, err := orch.Execute(ctx, ExecutionRequest{Message: "verificationtask", Mode: ExecutionStreaming, VerificationMode: verification.ModeEnforce,
					VerificationObligations: []verification.Obligation{{ID: "build", Kind: verification.KindBuild, Command: "go build ./..."}},
				})
				if err != nil {
					t.Fatal(err)
				}
				for range result.Stream {
				}
			} else {
				input := make(chan *model.ChatResponse, 3)
				input <- &model.ChatResponse{Content: "partial", Delta: true}
				if scenario == "error" {
					input <- &model.ChatResponse{Err: errors.New("private provider payload must not be saved")}
					close(input)
				}
				output := orch.runtimeMemory.observeStream(ctx, input, "streamtask", cancel)
				<-output
				switch scenario {
				case "cancel":
					cancel()
				case "close":
					if err := orch.Close(); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan struct{})
				go func() {
					for range output {
					}
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("stream observer leaked")
				}
				if ctx.Err() == nil {
					t.Fatal("upstream stream not canceled when observer exited")
				}
			}
			store := orch.runtimeMemory.store
			if scenario == "close" {
				var err error
				store, err = memory.OpenLayerStore(context.Background(), filepath.Join(os.Getenv("CHRONOS_CODE_DATA_HOME"), "memory.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
			}
			records, err := store.Recall(context.Background(), orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject})
			if err != nil || len(records) != 1 {
				t.Fatalf("terminal episode missing: %+v %v", records, err)
			}
			if strings.Contains(records[0].Content, "Outcome (completed)") || strings.Contains(records[0].Content, "private provider") {
				t.Fatalf("incorrect failure episode: %+v", records[0])
			}
		})
	}
}
