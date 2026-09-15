package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func runtimeCLIProject(t *testing.T) (config.ProjectPaths, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	// A worktree's .git is a file, not a directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := config.ResolveProjectPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	return paths, subdir
}

func TestCLILegacyMemoryRootAndSubdirectory(t *testing.T) {
	paths, subdir := runtimeCLIProject(t)
	store := memory.NewStore(filepath.Join(paths.LegacyDir, "memory"))
	record, err := store.Add(memory.CategoryProject, "root convention")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, dir := range []string{paths.Root, subdir} {
		t.Chdir(dir)
		for _, args := range [][]string{{"list", "project"}, {"search", "root", "convention"}} {
			var out bytes.Buffer
			if err := runMemoryCommand(ctx, &config.Config{}, args, &out); err != nil || !strings.Contains(out.String(), record.ID) {
				t.Fatalf("%s %v: %q, %v", dir, args, out.String(), err)
			}
		}
	}
	var out bytes.Buffer
	if err := runMemoryCommand(storage.WithTenant(ctx, "other"), &config.Config{}, []string{"forget", record.ID}, &out); err == nil {
		t.Fatal("legacy forget crossed tenant boundary")
	}
	if err := runMemoryCommand(ctx, &config.Config{}, []string{"forget", record.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if records, err := store.List(""); err != nil || len(records) != 0 {
		t.Fatalf("root note not forgotten: %+v %v", records, err)
	}
	if _, err := os.Stat(filepath.Join(subdir, config.ConfigDirName)); !os.IsNotExist(err) {
		t.Fatalf("created subdirectory state: %v", err)
	}
}

func TestCLILearningTelemetryFallbackAndCanonicalPrecedence(t *testing.T) {
	paths, subdir := runtimeCLIProject(t)
	t.Chdir(subdir)
	ctx := context.Background()
	cfg := &config.Config{}
	check := func(want string) {
		t.Helper()
		store, root, err := openWorkspaceLearningStore(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close(ctx)
		got, err := store.Candidates(ctx, root)
		if err != nil || root != paths.Root || (want == "" && len(got) != 0) || (want != "" && (len(got) != 1 || got[0].TriggerHash != want)) {
			t.Fatalf("root=%q candidates=%+v err=%v, want %q", root, got, err, want)
		}
	}
	check("")
	if _, err := os.Stat(paths.TelemetryDB); !os.IsNotExist(err) {
		t.Fatalf("empty analytics created canonical DB: %v", err)
	}
	seed := func(path, hash string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		store, err := learning.OpenSQLStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close(ctx)
		if err := store.SaveCandidates(ctx, []learning.PatternCandidate{{RepoPath: paths.Root, TriggerHash: hash, SuccessCount: 3}}); err != nil {
			t.Fatal(err)
		}
	}
	seed(filepath.Join(paths.LegacyDir, "memory.db"), "legacy")
	check("legacy")
	if _, err := os.Stat(paths.TelemetryDB); !os.IsNotExist(err) {
		t.Fatalf("legacy fallback created canonical DB: %v", err)
	}
	seed(paths.TelemetryDB, "canonical")
	for _, dir := range []string{paths.Root, subdir} {
		t.Chdir(dir)
		check("canonical")
	}
}

func TestCLISessionsCanonicalMigrationAndExplicitPath(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			paths, subdir := runtimeCLIProject(t)
			if err := os.MkdirAll(paths.LegacyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(paths.LegacyDir, "sessions.db")
			wantDB := paths.SessionsDB
			configYAML := "{}\n"
			if explicit {
				source = filepath.Join(paths.Root, "custom.db")
				wantDB = source
				configYAML = "defaults:\n  storage:\n    backend: sqlite\n    dsn: custom.db\n"
			}
			configFile := filepath.Join(paths.LegacyDir, "config.yaml")
			if err := os.WriteFile(configFile, []byte(configYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			store, err := sqlite.New(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := session.NewManager(store, source).Ensure(ctx, "runtime-session", "coder"); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{paths.Root, subdir} {
				t.Chdir(dir)
				export := filepath.Join(t.TempDir(), "session.json")
				resetGlobalFlags(t, []string{"chronos-code", "session", "export", "runtime-session", export})
				configPath = configFile
				if err := runSession(); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(export)
				if err != nil || !bytes.Contains(data, []byte("runtime-session")) {
					t.Fatalf("session export: %s %v", data, err)
				}
			}
			if _, err := os.Stat(wantDB); err != nil {
				t.Fatalf("runtime DB missing: %v", err)
			}
			if explicit {
				if _, err := os.Stat(paths.SessionsDB); !os.IsNotExist(err) {
					t.Fatalf("explicit path opened default DB: %v", err)
				}
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("source DB not retained: %v", err)
			}
		})
	}
}

func TestCLILearnedYAMLRootAndOverrides(t *testing.T) {
	paths, subdir := runtimeCLIProject(t)
	for _, output := range []string{"", "custom/learned", filepath.Join(t.TempDir(), "learned")} {
		t.Run(output, func(t *testing.T) {
			dir := output
			if dir == "" {
				dir = defaultLearningOutputDir
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(paths.Root, dir)
			}
			store := learning.NewStore(dir)
			if err := store.Save(&learning.Suggestion{ID: "approved", Kind: "pattern", Title: "root pattern"}); err != nil {
				t.Fatal(err)
			}
			configFile := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configFile, []byte(fmt.Sprintf("learning:\n  output_dir: %q\n", output)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(subdir)
			if got, err := runLearnForTest(t, configFile, "list"); err != nil || !strings.Contains(got, "root pattern") {
				t.Fatalf("list: %q %v", got, err)
			}
			if _, err := runLearnForTest(t, configFile, "accept", "approved"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "patterns.yaml")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCLILayersIsolation(t *testing.T) {
	paths, subdir := runtimeCLIProject(t)
	ctx := context.Background()
	store, err := memory.OpenLayerStore(ctx, filepath.Join(filepath.Dir(filepath.Dir(paths.Dir)), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	options := memory.LayerOptions{ProjectID: paths.ID, UserID: fmt.Sprintf("uid:%d", os.Getuid()), OrganizationID: "org-a", SessionID: "seed", SemanticEnabled: true}
	seed := func(ctx context.Context, opts memory.LayerOptions, scope memory.Scope, kind memory.Kind, content string) memory.LayerRecord {
		t.Helper()
		input := memory.LayerRecordInput{Scope: scope, Kind: kind, Content: content, Source: "test", Revision: "v1", Publish: scope == memory.ScopeOrganization}
		if kind == memory.KindProcedural {
			input.Steps = []string{"test first"}
		}
		r, err := store.Record(ctx, opts, input)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	project := seed(ctx, options, memory.ScopeProject, memory.KindEpisodic, "visible episode")
	seed(ctx, options, memory.ScopeProject, memory.KindProcedural, "visible procedure")
	seed(ctx, options, memory.ScopeProject, memory.KindSemantic, "semantic fact")
	seed(ctx, options, memory.ScopeUser, memory.KindEpisodic, "user episode")
	seed(ctx, options, memory.ScopeOrganization, memory.KindOrganizational, "org standard")
	seed(ctx, options, memory.ScopeTenant, memory.KindEpisodic, "tenant episode")
	seed(storage.WithTenant(ctx, "other"), options, memory.ScopeProject, memory.KindEpisodic, "hidden tenant")
	other := options
	other.ProjectID, other.UserID, other.OrganizationID = "other-project", "other-user", "other-org"
	foreign := seed(ctx, other, memory.ScopeProject, memory.KindEpisodic, "hidden project")
	seed(ctx, other, memory.ScopeUser, memory.KindEpisodic, "hidden user")
	seed(ctx, other, memory.ScopeOrganization, memory.KindOrganizational, "hidden org")
	cfg := &config.Config{Memory: config.MemoryConfig{OrganizationID: "org-a"}}
	run := func(ctx context.Context, args ...string) (string, error) {
		var out bytes.Buffer
		err := runMemoryCommand(ctx, cfg, append([]string{"layers"}, args...), &out)
		return out.String(), err
	}
	for _, dir := range []string{paths.Root, subdir} {
		t.Chdir(dir)
		got, err := run(ctx, "list", "--scope", "project")
		if err != nil || !strings.Contains(got, project.ID) || strings.Contains(got, "hidden") || strings.Contains(got, "semantic fact") {
			t.Fatalf("project list: %q %v", got, err)
		}
		got, err = run(ctx, "search", "visible", "--scope=project", "--kind=procedural")
		if err != nil || !strings.Contains(got, "visible procedure") || strings.Contains(got, "visible episode") {
			t.Fatalf("kind search: %q %v", got, err)
		}
	}
	for scope, want := range map[string]string{"user": "user episode", "organization": "org standard", "tenant": "tenant episode"} {
		got, err := run(ctx, "list", "--scope", scope)
		if err != nil || !strings.Contains(got, want) || strings.Contains(got, "hidden") {
			t.Fatalf("%s list: %q %v", scope, got, err)
		}
	}
	got, err := run(storage.WithTenant(ctx, "other"), "list", "--scope=project")
	if err != nil || !strings.Contains(got, "hidden tenant") || strings.Contains(got, "visible") {
		t.Fatalf("context tenant list: %q %v", got, err)
	}
	// User and organization records share one DB across distinct projects.
	cfg.Workspace.Root = t.TempDir()
	for scope, want := range map[string]string{"user": "user episode", "organization": "org standard"} {
		got, err := run(ctx, "list", "--scope", scope)
		if err != nil || !strings.Contains(got, want) || strings.Contains(got, "hidden") {
			t.Fatalf("cross-project %s list: %q %v", scope, got, err)
		}
	}
	got, err = run(ctx, "list", "--scope=project")
	if err != nil || strings.TrimSpace(got) != "[]" {
		t.Fatalf("project scope crossed projects: %q %v", got, err)
	}
	cfg.Workspace.Root = ""
	for _, args := range [][]string{{"list"}, {"list", "--scope=all"}, {"list", "--scope=project", "--kind=bad"}, {"list", "--scope=project", "--kind=semantic"}, {"list", "--scope=project", "--user-id=other"}, {"forget", project.ID, "--scope=project", "--kind=episodic"}} {
		if _, err := run(ctx, args...); err == nil {
			t.Fatalf("accepted invalid command %v", args)
		}
	}
	if _, err := run(ctx, "forget", foreign.ID, "--scope=project"); !errors.Is(err, memory.ErrLayerNotFound) {
		t.Fatalf("foreign forget: %v", err)
	}
	if _, err := run(storage.WithTenant(ctx, "other"), "forget", project.ID, "--scope=project"); !errors.Is(err, memory.ErrLayerNotFound) {
		t.Fatalf("cross tenant forget: %v", err)
	}
	if _, err := run(ctx, "forget", project.ID, "--scope=project"); err != nil {
		t.Fatal(err)
	}
	got, err = run(ctx, "list", "--scope=project")
	if err != nil || strings.Contains(got, project.ID) {
		t.Fatalf("forgotten entry still visible: %q %v", got, err)
	}
	cfg.Memory.OrganizationID = ""
	if _, err := run(ctx, "list", "--scope=organization"); err == nil {
		t.Fatal("organization scope allowed without opt-in")
	}
	enabled := true
	cfg.Memory.SemanticEnabled = &enabled
	got, err = run(ctx, "list", "--scope=project", "--kind=semantic")
	if err != nil || !strings.Contains(got, "semantic fact") {
		t.Fatalf("semantic opt-in: %q %v", got, err)
	}
}
