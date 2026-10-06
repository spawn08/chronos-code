package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func seedSession(t *testing.T, dbPath, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.CreateSession(context.Background(), &storage.Session{ID: id, AgentID: "a", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestLocateResumeSessionAcrossProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHRONOS_CODE_DATA_HOME", home)
	other := filepath.Join(home, "projects", "elsewhere-abc", "sessions.db")
	seedSession(t, other, "sess_found")

	cfg := &config.Config{}
	if err := locateResumeSession(context.Background(), cfg, "sess_found"); err != nil {
		t.Fatalf("locate: %v", err)
	}
	if cfg.SessionsDBOverride != other {
		t.Fatalf("override = %q, want %q", cfg.SessionsDBOverride, other)
	}

	cfg = &config.Config{}
	err := locateResumeSession(context.Background(), cfg, "sess_missing")
	if !errors.Is(err, errSessionNotFound) {
		t.Fatalf("err = %v, want errSessionNotFound", err)
	}
	if cfg.SessionsDBOverride != "" {
		t.Fatalf("override set on miss: %q", cfg.SessionsDBOverride)
	}
}

func TestLocateResumeSessionPrefersCurrentProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHRONOS_CODE_DATA_HOME", home)
	cfg := &config.Config{}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, paths.SessionsDB, "sess_here")
	if err := locateResumeSession(context.Background(), cfg, "sess_here"); err != nil {
		t.Fatal(err)
	}
	if cfg.SessionsDBOverride != "" {
		t.Fatalf("override = %q, want none for current project", cfg.SessionsDBOverride)
	}
}

func TestSplitModelProviderPrefix(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		in, provider, model string
		ok                  bool
	}{
		{"azure/my-deploy", "azure", "my-deploy", true},
		{"anthropic/claude-sonnet-4-5", "anthropic", "claude-sonnet-4-5", true},
		{"azure-openai/x", "azure", "x", true},
		{"claude-sonnet-4-5", "", "", false},
		{"meta-llama/Llama-3", "", "", false},
		{"azure/", "", "", false},
	} {
		p, m, ok := splitModelProviderPrefix(tc.in, cfg)
		if p != tc.provider || m != tc.model || ok != tc.ok {
			t.Errorf("%q -> %q %q %v", tc.in, p, m, ok)
		}
	}
}

func TestVersionInfoCapabilities(t *testing.T) {
	info := currentVersionInfo()
	want := map[string]bool{"stream-json": false, "mcp-http": false, "mcp-config": false, "prompt-stdin": false, "system-prompt": false, "max-turns": false, "thinking": false, "ephemeral": false, "mcp-status": false, "require-mcp": false, "skill-sources": false, "project-docs-budget": false, "policy-file": false}
	for _, c := range info.Capabilities {
		want[c] = true
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("capability %q missing", name)
		}
	}
}
