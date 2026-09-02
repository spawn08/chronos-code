//go:build !lsp

package lsp

import (
	"fmt"

	"github.com/spawn08/chronos/engine/tool"
)

// Client is a stub when the lsp build tag is not set.
type Client struct{}

// Manager is a stub when the lsp build tag is not set.
type Manager struct{}

// NewClient returns an error when the lsp build tag is not set.
func NewClient(command string, args ...string) (*Client, error) {
	return nil, fmt.Errorf("lsp: not compiled (build with -tags lsp)")
}

// Close is a no-op stub.
func (c *Client) Close() error { return nil }

// NewManager returns an inert manager when the lsp build tag is not set.
func NewManager(root string) *Manager { return &Manager{} }

// Close is a no-op stub.
func (m *Manager) Close() error { return nil }

// Tools returns nil when the lsp build tag is not set.
func Tools(manager *Manager, root string) []*tool.Definition { return nil }
