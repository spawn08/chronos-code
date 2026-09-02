//go:build lsp

package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
)

func TestToolsRegisterEstablishedNames(t *testing.T) {
	manager := toolTestManager(t.TempDir(), &toolTestClient{})
	definitions := Tools(manager, t.TempDir())
	if len(definitions) != 4 {
		t.Fatalf("Tools returned %d definitions, want 4", len(definitions))
	}
	names := make([]string, len(definitions))
	for i, definition := range definitions {
		names[i] = definition.Name
	}
	sort.Strings(names)
	want := []string{"lsp_diagnostics", "lsp_hover", "lsp_references", "lsp_rename_preview"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool names=%v, want %v", names, want)
	}
}

func TestWorkspaceRejectsTraversalAndSymlinkEscapeBeforeServer(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.go")
	if err := os.WriteFile(outsideFile, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "escape.go")); err != nil {
		t.Fatal(err)
	}

	client := &toolTestClient{}
	lookups := 0
	manager := NewManager(root,
		WithExecutableLookup(func(command string) (string, error) {
			lookups++
			return command, nil
		}),
		WithClientStart(func(string, ...string) (ManagedClient, error) { return client, nil }),
	)
	diagnostics := definitionNamed(t, Tools(manager, root), "lsp_diagnostics")
	for _, file := range []string{filepath.Join("..", filepath.Base(outside), "outside.go"), "escape.go", outsideFile} {
		if _, err := diagnostics.Handler(context.Background(), map[string]any{"file": file}); err == nil {
			t.Errorf("file %q should be rejected", file)
		}
	}
	if lookups != 0 {
		t.Fatalf("server lookups=%d, want 0", lookups)
	}
}

func TestToolsValidatePositionsBeforeServerCalls(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	manager := NewManager(root,
		WithExecutableLookup(func(command string) (string, error) {
			lookups++
			return command, nil
		}),
		WithClientStart(func(string, ...string) (ManagedClient, error) { return &toolTestClient{}, nil }),
	)
	definitions := Tools(manager, root)
	tests := []struct {
		name string
		args map[string]any
	}{
		{"lsp_hover", map[string]any{"file": "main.go", "line": 0, "character": 1}},
		{"lsp_references", map[string]any{"file": "main.go", "line": 1, "character": -1}},
		{"lsp_hover", map[string]any{"file": "main.go", "line": 1.5, "character": 1}},
		{"lsp_rename_preview", map[string]any{"file": "main.go", "line": 1, "character": 1}},
	}
	for _, test := range tests {
		if _, err := definitionNamed(t, definitions, test.name).Handler(context.Background(), test.args); err == nil {
			t.Errorf("%s with args %v should fail", test.name, test.args)
		}
	}
	if lookups != 0 {
		t.Fatalf("server lookups=%d, want 0", lookups)
	}
}

func TestToolsUseDidOpenThenDidChange(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &toolTestClient{hover: &HoverResult{Contents: "main"}}
	manager := toolTestManager(root, client)
	hover := definitionNamed(t, Tools(manager, root), "lsp_hover")
	args := map[string]any{"file": "main.go", "line": float64(1), "character": float64(1)}
	if _, err := hover.Handler(context.Background(), args); err != nil {
		t.Fatalf("first hover: %v", err)
	}
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hover.Handler(context.Background(), args); err != nil {
		t.Fatalf("second hover: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.openCalls != 1 || client.changeCalls != 1 {
		t.Fatalf("DidOpen calls=%d, DidChange calls=%d; want 1 each", client.openCalls, client.changeCalls)
	}
	if client.version != 2 || client.text != "package main\n\nfunc main() {}\n" {
		t.Fatalf("changed document version=%d text=%q", client.version, client.text)
	}
	if client.hoverLine != 0 || client.hoverCol != 0 {
		t.Fatalf("hover position=(%d,%d), want (0,0)", client.hoverLine, client.hoverCol)
	}
}

func TestDiagnosticsAreSortedCappedAndExpired(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	diagnostics := make([]Diagnostic, 0, diagnosticLimit+3)
	diagnostics = append(diagnostics, Diagnostic{
		Severity: 1, Message: "stale", ReceivedAt: now.Add(-diagnosticTTL - time.Second),
	})
	for i := diagnosticLimit + 1; i >= 0; i-- {
		diagnostics = append(diagnostics, Diagnostic{
			Severity:   2,
			Message:    fmt.Sprintf("warning-%02d", i),
			Range:      Range{Start: Position{Line: i, Character: i}},
			ReceivedAt: now,
		})
	}
	diagnostics = append(diagnostics, Diagnostic{
		Severity: 1, Message: "first error", Range: Range{Start: Position{Line: 4}}, ReceivedAt: now,
	})
	client := &toolTestClient{diagnostics: diagnostics}
	definition := definitionNamed(t, Tools(toolTestManager(root, client), root), "lsp_diagnostics")
	result, err := definition.Handler(context.Background(), map[string]any{"file": "main.go"})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	output := result.(map[string]any)
	if output["count"] != diagnosticLimit {
		t.Fatalf("count=%v, want %d", output["count"], diagnosticLimit)
	}
	items := output["diagnostics"].([]map[string]any)
	if items[0]["message"] != "first error" {
		t.Fatalf("first diagnostic=%v, want first error", items[0])
	}
	for _, item := range items {
		if item["message"] == "stale" {
			t.Fatal("stale diagnostic was returned")
		}
	}
}

func definitionNamed(t *testing.T, definitions []*tool.Definition, name string) *tool.Definition {
	t.Helper()
	for _, definition := range definitions {
		if definition.Name == name {
			return definition
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func toolTestManager(root string, client ManagedClient) *Manager {
	return NewManager(root,
		WithExecutableLookup(func(command string) (string, error) { return command, nil }),
		WithClientStart(func(string, ...string) (ManagedClient, error) { return client, nil }),
	)
}

type toolTestClient struct {
	mu sync.Mutex

	openCalls   int
	changeCalls int
	version     int
	text        string
	hoverLine   int
	hoverCol    int

	diagnostics []Diagnostic
	hover       *HoverResult
}

func (c *toolTestClient) Initialize(context.Context, string) error { return nil }
func (c *toolTestClient) DidOpen(_ context.Context, _ string, _ string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openCalls++
	c.version = 1
	c.text = text
	return nil
}
func (c *toolTestClient) DidChange(_ context.Context, _ string, version int, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changeCalls++
	c.version = version
	c.text = text
	return nil
}
func (c *toolTestClient) Diagnostics(context.Context, string) ([]Diagnostic, error) {
	return append([]Diagnostic(nil), c.diagnostics...), nil
}
func (c *toolTestClient) Hover(_ context.Context, _ string, line, col int) (*HoverResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hoverLine = line
	c.hoverCol = col
	return c.hover, nil
}
func (c *toolTestClient) References(context.Context, string, int, int) ([]Location, error) {
	return nil, nil
}
func (c *toolTestClient) RenamePreview(context.Context, string, int, int, string) (*WorkspaceEdit, error) {
	return nil, nil
}
func (c *toolTestClient) Close() error { return nil }
