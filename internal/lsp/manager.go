//go:build lsp

package lsp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ManagedClient is the client contract used by Manager.
type ManagedClient interface {
	Initialize(context.Context, string) error
	DidOpen(context.Context, string, string, string) error
	Diagnostics(context.Context, string) ([]Diagnostic, error)
	Hover(context.Context, string, int, int) (*HoverResult, error)
	References(context.Context, string, int, int) ([]Location, error)
	RenamePreview(context.Context, string, int, int, string) (*WorkspaceEdit, error)
	Close() error
}

// LookupFunc resolves a language-server executable.
type LookupFunc func(string) (string, error)

// StartFunc starts a language-server client.
type StartFunc func(string, ...string) (ManagedClient, error)

// ManagerOption configures a Manager dependency.
type ManagerOption func(*Manager)

// WithExecutableLookup replaces executable discovery, primarily for tests.
func WithExecutableLookup(lookup LookupFunc) ManagerOption {
	return func(m *Manager) { m.lookup = lookup }
}

// WithClientStart replaces language-server startup, primarily for tests.
func WithClientStart(start StartFunc) ManagerOption {
	return func(m *Manager) { m.start = start }
}

type serverSpec struct {
	language string
	command  string
	args     []string
}

// Manager lazily owns at most one initialized client for each supported
// language server.
type Manager struct {
	root   string
	lookup LookupFunc
	start  StartFunc

	mu      sync.Mutex
	clients map[string]ManagedClient
	closed  bool

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// NewManager creates a lazy multi-language LSP manager for root.
func NewManager(root string, options ...ManagerOption) *Manager {
	m := &Manager{
		root:      root,
		lookup:    exec.LookPath,
		start:     defaultStart,
		clients:   make(map[string]ManagedClient),
		closeDone: make(chan struct{}),
	}
	for _, option := range options {
		option(m)
	}
	return m
}

func defaultStart(command string, args ...string) (ManagedClient, error) {
	return NewClient(command, args...)
}

// ResolveServer maps a workspace language label, extension, or path to its
// canonical language-server invocation.
func ResolveServer(languageOrPath string) (language, command string, args []string, ok bool) {
	value := strings.ToLower(strings.TrimSpace(languageOrPath))
	if ext := strings.ToLower(filepath.Ext(value)); ext != "" {
		value = ext
	}

	var spec serverSpec
	switch strings.TrimPrefix(value, ".") {
	case "go", "golang":
		spec = serverSpec{language: "go", command: "gopls"}
	case "js", "jsx", "javascript", "javascriptreact", "ts", "tsx", "typescript", "typescriptreact":
		spec = serverSpec{language: "typescript", command: "typescript-language-server", args: []string{"--stdio"}}
	case "py", "python":
		spec = serverSpec{language: "python", command: "pyright-langserver", args: []string{"--stdio"}}
	case "rs", "rust":
		spec = serverSpec{language: "rust", command: "rust-analyzer"}
	default:
		return "", "", nil, false
	}
	return spec.language, spec.command, append([]string(nil), spec.args...), true
}

// ClientFor returns the initialized client for a language label, extension,
// or file path. Unsupported languages and missing executables are unavailable,
// not errors.
func (m *Manager) ClientFor(ctx context.Context, languageOrPath string) (ManagedClient, bool, error) {
	language, command, args, ok := ResolveServer(languageOrPath)
	if !ok {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, fmt.Errorf("lsp: manager is closed")
	}
	if client := m.clients[language]; client != nil {
		return client, true, nil
	}

	executable, err := m.lookup(command)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lsp: find %s: %w", command, err)
	}
	client, err := m.start(executable, args...)
	if err != nil {
		return nil, false, fmt.Errorf("lsp: start %s: %w", command, err)
	}
	if client == nil {
		return nil, false, fmt.Errorf("lsp: start %s returned a nil client", command)
	}
	if err := client.Initialize(ctx, m.root); err != nil {
		closeErr := client.Close()
		return nil, false, errors.Join(fmt.Errorf("lsp: initialize %s: %w", command, err), closeErr)
	}
	m.clients[language] = client
	return client, true, nil
}

// Close closes every initialized client once in deterministic language order.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		languages := make([]string, 0, len(m.clients))
		for language := range m.clients {
			languages = append(languages, language)
		}
		sort.Strings(languages)
		clients := make([]ManagedClient, len(languages))
		for i, language := range languages {
			clients[i] = m.clients[language]
		}
		m.mu.Unlock()

		errs := make([]error, 0, len(clients))
		for i, client := range clients {
			if err := client.Close(); err != nil {
				errs = append(errs, fmt.Errorf("lsp: close %s: %w", languages[i], err))
			}
		}
		m.closeErr = errors.Join(errs...)
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}
