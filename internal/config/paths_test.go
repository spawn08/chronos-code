package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProjectPathsIdentity(t *testing.T) {
	base := t.TempDir()
	dataHome := filepath.Join(base, "data")
	t.Setenv("CHRONOS_CODE_DATA_HOME", dataHome)
	repo := filepath.Join(base, "clone-a", "repo")
	clone := filepath.Join(base, "clone-b", "repo")
	worktree := filepath.Join(base, "worktree")
	plain := filepath.Join(base, "plain")
	for _, dir := range []string{repo, clone, worktree, plain} {
		if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{repo, clone} {
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../clone-a/repo/.git/worktrees/worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "alias")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, input, root string }{
		{"root", repo, repo},
		{"subdirectory", filepath.Join(repo, "src", "nested"), repo},
		{"symlink", link, repo},
		{"symlink subdirectory", filepath.Join(link, "src"), repo},
		{"separate clone", clone, clone},
		{"worktree git file", worktree, worktree},
		{"worktree subdirectory", filepath.Join(worktree, "src"), worktree},
		{"without git", plain, plain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveProjectPaths(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(tt.root)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(root))
			id := fmt.Sprintf("%s-%x", filepath.Base(root), hash[:16])
			dir := filepath.Join(dataHome, "projects", id)
			want := ProjectPaths{Root: root, ID: id, Dir: dir,
				SessionsDB: filepath.Join(dir, "sessions.db"), GraphDB: filepath.Join(dir, "graph.db"),
				PlansDB:     filepath.Join(dir, "plans.db"),
				TelemetryDB: filepath.Join(dir, "telemetry.db"), MemoryDB: filepath.Join(dir, "memory.db"),
				LegacyDir: filepath.Join(root, ConfigDirName)}
			if got != want {
				t.Fatalf("paths = %+v, want %+v", got, want)
			}
			t.Chdir(tt.input)
			if gotRoot := WorkspaceRoot(); gotRoot != root {
				t.Fatalf("WorkspaceRoot = %q, want %q", gotRoot, root)
			}
			fromCWD, err := ResolveProjectPaths("")
			if err != nil || fromCWD != got {
				t.Fatalf("CWD paths = %+v, %v; want %+v", fromCWD, err, got)
			}
		})
	}
	if _, err := os.Stat(dataHome); !os.IsNotExist(err) {
		t.Fatalf("resolution created data home: %v", err)
	}
}

func TestProjectPathsDataHome(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tt := range []struct{ name, env, want string }{
		{"default", "", filepath.Join(home, ConfigDirName)},
		{"absolute", filepath.Join(home, "data"), filepath.Join(home, "data")},
		{"relative to root", "data", filepath.Join(root, "data")},
		{"home expansion", "~/data", filepath.Join(home, "data")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHRONOS_CODE_DATA_HOME", tt.env)
			paths, err := ResolveProjectPaths(root)
			if err != nil {
				t.Fatal(err)
			}
			// The project root is canonical, including macOS /var -> /private/var.
			want := tt.want
			if tt.env == "data" {
				want = filepath.Join(paths.Root, "data")
			}
			if paths.Dir != filepath.Join(want, "projects", paths.ID) {
				t.Fatalf("Dir = %q, data home = %q", paths.Dir, want)
			}
		})
	}
}

func TestConfigProjectPathsOverrides(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHRONOS_CODE_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	defaults, err := ResolveProjectPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "sessions.db")
	for _, tt := range []struct{ name, overlay, sessions, graph string }{
		{"embedded legacy defaults", "", defaults.SessionsDB, defaults.GraphDB},
		{"relative", "defaults:\n  storage:\n    dsn: db/custom.db\nworkspace:\n  graph_db: db/index.db\n", filepath.Join(defaults.Root, "db/custom.db"), filepath.Join(defaults.Root, "db/index.db")},
		{"explicit legacy defaults", "defaults:\n  storage:\n    dsn: .chronos-code/sessions.db\nworkspace:\n  graph_db: .chronos-code/graph.db\n", filepath.Join(defaults.LegacyDir, "sessions.db"), filepath.Join(defaults.LegacyDir, "graph.db")},
		{"absolute", fmt.Sprintf("defaults:\n  storage:\n    dsn: %q\nworkspace:\n  graph_db: %q\n", abs, abs), abs, abs},
		{"empty resets defaults", "defaults:\n  storage:\n    dsn: ''\nworkspace:\n  graph_db: ''\n", defaults.SessionsDB, defaults.GraphDB},
		{"sqlite memory", "defaults:\n  storage:\n    dsn: ':memory:'\n", ":memory:", defaults.GraphDB},
		{"sqlite URI", "defaults:\n  storage:\n    dsn: 'file:shared?mode=memory&cache=shared'\n", "file:shared?mode=memory&cache=shared", defaults.GraphDB},
		{"postgres DSN", "defaults:\n  storage:\n    backend: postgres\n    dsn: 'host=localhost dbname=chronos'\n", "host=localhost dbname=chronos", defaults.GraphDB},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadEmbeddedDefaults()
			if err != nil {
				t.Fatal(err)
			}
			if tt.overlay != "" {
				var overlay Config
				if err := yaml.Unmarshal([]byte(tt.overlay), &overlay); err != nil {
					t.Fatal(err)
				}
				mergeConfig(cfg, &overlay, "user")
				var unrelated Config
				if err := yaml.Unmarshal([]byte("defaults:\n  model:\n    model: project-model\n"), &unrelated); err != nil {
					t.Fatal(err)
				}
				mergeConfig(cfg, &unrelated, "project")
			}
			before, err := cfg.EffectiveConfig()
			if err != nil {
				t.Fatal(err)
			}
			got, err := cfg.ResolveProjectPaths(root)
			if err != nil {
				t.Fatal(err)
			}
			if got.SessionsDB != tt.sessions || got.GraphDB != tt.graph {
				t.Fatalf("paths = %+v, want sessions %q graph %q", got, tt.sessions, tt.graph)
			}
			after, err := cfg.EffectiveConfig()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("resolution mutated config: %v", err)
			}
		})
	}
}

func TestMemoryOptionsYAMLMerge(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Memory.Enabled || !cfg.Memory.LayeredMemoryEnabled() || cfg.Memory.SemanticMemoryEnabled() {
		t.Fatalf("unexpected memory defaults: %+v", cfg.Memory)
	}
	for _, tt := range []struct {
		name, text        string
		layered, semantic bool
		organization      string
		budget, records   int
	}{
		{"user", "memory:\n  layered_enabled: false\n  semantic_enabled: true\n  organization_id: example\n  context_budget_tokens: 2048\n  max_records: 12\n", false, true, "example", 2048, 12},
		{"project", "memory:\n  backend: yaml\n", false, true, "example", 2048, 12},
		{"cli", "memory:\n  layered_enabled: true\n  semantic_enabled: false\n  organization_id: ''\n  context_budget_tokens: 0\n  max_records: 0\n", true, false, "", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var overlay Config
			if err := yaml.Unmarshal([]byte(tt.text), &overlay); err != nil {
				t.Fatal(err)
			}
			mergeConfig(cfg, &overlay, tt.name)
			m := cfg.Memory
			if m.LayeredMemoryEnabled() != tt.layered || m.SemanticMemoryEnabled() != tt.semantic || m.OrganizationID != tt.organization || m.ContextBudgetTokens != tt.budget || m.MaxRecords != tt.records {
				t.Fatalf("memory = %+v", m)
			}
			if !m.Enabled || !m.AutoExtract || m.Backend != "yaml" {
				t.Fatalf("legacy memory options lost: %+v", m)
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip Config
			if err := yaml.Unmarshal(data, &roundTrip); err != nil || !reflect.DeepEqual(roundTrip.Memory, m) {
				t.Fatalf("memory YAML round trip = %+v, %v", roundTrip.Memory, err)
			}
		})
	}
}

func TestProjectPathsInvalidRoot(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(base, "missing"), file} {
		t.Run(filepath.Base(root), func(t *testing.T) {
			if _, err := ResolveProjectPaths(root); err == nil {
				t.Fatal("expected invalid root error")
			}
		})
	}
}

func TestLoadProjectPaths(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "repo")
	dataHome := filepath.Join(base, "data")
	for _, dir := range []string{filepath.Join(home, ConfigDirName), filepath.Join(root, ConfigDirName), filepath.Join(root, "src"), filepath.Join(root, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CHRONOS_CODE_DATA_HOME", dataHome)
	for path, text := range map[string]string{
		filepath.Join(home, ConfigDirName, "config.yaml"): "defaults:\n  storage:\n    dsn: .chronos-code/sessions.db\nmemory:\n  semantic_enabled: true\n  organization_id: example\n",
		filepath.Join(root, ConfigDirName, "config.yaml"): "defaults:\n  model:\n    model: project-model\nworkspace:\n  graph_db: custom/graph.db\n",
		filepath.Join(base, "cli.yaml"):                   "memory:\n  semantic_enabled: false\n",
		filepath.Join(root, ConfigDirName, "sessions.db"): "legacy session sentinel",
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(root, "src"))
	cfg, err := Load(filepath.Join(base, "cli.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if paths.SessionsDB != filepath.Join(paths.LegacyDir, "sessions.db") || paths.GraphDB != filepath.Join(paths.Root, "custom/graph.db") {
		t.Fatalf("loaded paths = %+v", paths)
	}
	if cfg.Memory.SemanticMemoryEnabled() || cfg.Memory.OrganizationID != "example" || !cfg.Memory.LayeredMemoryEnabled() {
		t.Fatalf("loaded memory = %+v", cfg.Memory)
	}
	if cfg.sources["defaults.storage.dsn"] != "user" || cfg.sources["workspace.graph_db"] != "project" || cfg.sources["memory.semantic_enabled"] != "cli" {
		t.Fatalf("incorrect path/memory provenance: %v", cfg.sources)
	}
	data, err := os.ReadFile(paths.SessionsDB)
	if err != nil || string(data) != "legacy session sentinel" {
		t.Fatalf("legacy data changed: %q, %v", data, err)
	}
	if _, err := os.Stat(dataHome); !os.IsNotExist(err) {
		t.Fatalf("Load/resolution created data home: %v", err)
	}
	// A caller-supplied root and workspace overrides must not depend on CWD.
	t.Chdir(home)
	for _, workspaceRoot := range []string{"", root, "repo"} {
		cfg.Workspace.Root = workspaceRoot
		input := root
		if workspaceRoot == "repo" {
			input = base
		}
		got, err := cfg.ResolveProjectPaths(input)
		if err != nil || got != paths {
			t.Fatalf("workspace root %q: %+v, %v; want %+v", workspaceRoot, got, err, paths)
		}
	}
}
