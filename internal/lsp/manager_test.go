//go:build lsp

package lsp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"sync"
	"testing"
)

func TestManagerResolveServer(t *testing.T) {
	tests := []struct {
		input    string
		language string
		command  string
		args     []string
	}{
		{"go", "go", "gopls", nil},
		{"main.go", "go", "gopls", nil},
		{"javascript", "typescript", "typescript-language-server", []string{"--stdio"}},
		{"component.JSX", "typescript", "typescript-language-server", []string{"--stdio"}},
		{"typescriptreact", "typescript", "typescript-language-server", []string{"--stdio"}},
		{"component.tsx", "typescript", "typescript-language-server", []string{"--stdio"}},
		{"python", "python", "pyright-langserver", []string{"--stdio"}},
		{"script.py", "python", "pyright-langserver", []string{"--stdio"}},
		{"rust", "rust", "rust-analyzer", nil},
		{"lib.rs", "rust", "rust-analyzer", nil},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			language, command, args, ok := ResolveServer(tt.input)
			if !ok {
				t.Fatal("ResolveServer reported unsupported")
			}
			if language != tt.language || command != tt.command || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("ResolveServer(%q)=(%q, %q, %v), want (%q, %q, %v)", tt.input, language, command, args, tt.language, tt.command, tt.args)
			}
		})
	}
	if _, _, _, ok := ResolveServer("README.md"); ok {
		t.Fatal("ResolveServer should reject unsupported files")
	}
}

func TestManagerMissingExecutableIsUnavailable(t *testing.T) {
	starts := 0
	m := NewManager(t.TempDir(),
		WithExecutableLookup(func(string) (string, error) { return "", exec.ErrNotFound }),
		WithClientStart(func(string, ...string) (ManagedClient, error) {
			starts++
			return &managerTestClient{}, nil
		}),
	)

	client, available, err := m.ClientFor(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if available || client != nil {
		t.Fatalf("missing executable returned client=%v available=%t", client, available)
	}
	if starts != 0 {
		t.Fatalf("start calls=%d, want 0", starts)
	}
}

func TestManagerConcurrentClientForStartsOncePerLanguage(t *testing.T) {
	var mu sync.Mutex
	starts := make(map[string]int)
	clients := make(map[string]*managerTestClient)
	m := NewManager("/workspace",
		WithExecutableLookup(func(command string) (string, error) { return "/bin/" + command, nil }),
		WithClientStart(func(command string, _ ...string) (ManagedClient, error) {
			mu.Lock()
			defer mu.Unlock()
			starts[command]++
			client := &managerTestClient{}
			clients[command] = client
			return client, nil
		}),
	)

	inputs := []string{"main.go", "go", "app.js", "component.tsx", "python", "script.py", "rust", "lib.rs"}
	results := make([]ManagedClient, len(inputs))
	var wg sync.WaitGroup
	for i, input := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, available, err := m.ClientFor(context.Background(), input)
			if err != nil || !available {
				t.Errorf("ClientFor(%q): available=%t err=%v", input, available, err)
				return
			}
			results[i] = client
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, command := range []string{"/bin/gopls", "/bin/typescript-language-server", "/bin/pyright-langserver", "/bin/rust-analyzer"} {
		if starts[command] != 1 {
			t.Errorf("starts[%q]=%d, want 1", command, starts[command])
		}
		if clients[command].initializeCalls != 1 || clients[command].root != "/workspace" {
			t.Errorf("client %q initialization=(%d, %q), want (1, /workspace)", command, clients[command].initializeCalls, clients[command].root)
		}
	}
	if results[2] != results[3] {
		t.Fatal("JavaScript and TypeScript did not share one client")
	}
}

func TestManagerCloseAllExactlyOnceAndJoinsErrors(t *testing.T) {
	closeOrder := make([]string, 0, 4)
	var orderMu sync.Mutex
	closeErrors := map[string]error{
		"go":     errors.New("go close failed"),
		"python": errors.New("python close failed"),
	}
	m := NewManager("/workspace",
		WithExecutableLookup(func(command string) (string, error) { return command, nil }),
		WithClientStart(func(command string, _ ...string) (ManagedClient, error) {
			language, _, _, ok := ResolveServer(command)
			if !ok {
				switch command {
				case "gopls":
					language = "go"
				case "pyright-langserver":
					language = "python"
				case "rust-analyzer":
					language = "rust"
				case "typescript-language-server":
					language = "typescript"
				}
			}
			return &managerTestClient{name: language, closeErr: closeErrors[language], closeOrder: &closeOrder, orderMu: &orderMu}, nil
		}),
	)
	for _, language := range []string{"rust", "typescript", "python", "go"} {
		if _, available, err := m.ClientFor(context.Background(), language); err != nil || !available {
			t.Fatalf("ClientFor(%q): available=%t err=%v", language, available, err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.Close()
		}()
	}
	wg.Wait()

	wantOrder := []string{"go", "python", "rust", "typescript"}
	if !reflect.DeepEqual(closeOrder, wantOrder) {
		t.Fatalf("close order=%v, want %v", closeOrder, wantOrder)
	}
	for i, err := range errs {
		if !errors.Is(err, closeErrors["go"]) || !errors.Is(err, closeErrors["python"]) {
			t.Errorf("Close error %d=%v, want both close failures", i, err)
		}
	}
	if _, _, err := m.ClientFor(context.Background(), "go"); err == nil {
		t.Fatal("ClientFor after Close should fail")
	}
}

type managerTestClient struct {
	name            string
	root            string
	initializeCalls int
	closeCalls      int
	closeErr        error
	closeOrder      *[]string
	orderMu         *sync.Mutex
}

func (c *managerTestClient) Initialize(_ context.Context, root string) error {
	c.initializeCalls++
	c.root = root
	return nil
}

func (c *managerTestClient) DidOpen(context.Context, string, string, string) error { return nil }
func (c *managerTestClient) Diagnostics(context.Context, string) ([]Diagnostic, error) {
	return nil, nil
}
func (c *managerTestClient) Hover(context.Context, string, int, int) (*HoverResult, error) {
	return nil, nil
}
func (c *managerTestClient) References(context.Context, string, int, int) ([]Location, error) {
	return nil, nil
}
func (c *managerTestClient) RenamePreview(context.Context, string, int, int, string) (*WorkspaceEdit, error) {
	return nil, nil
}
func (c *managerTestClient) Close() error {
	c.closeCalls++
	if c.closeCalls != 1 {
		return fmt.Errorf("close called %d times", c.closeCalls)
	}
	if c.closeOrder != nil {
		c.orderMu.Lock()
		*c.closeOrder = append(*c.closeOrder, c.name)
		c.orderMu.Unlock()
	}
	return c.closeErr
}
