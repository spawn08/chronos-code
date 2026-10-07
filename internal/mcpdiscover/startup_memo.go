package mcpdiscover

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/spawn08/chronos/engine/mcp"
)

// StartupFailureMemo wraps a ClientFactory for one startup pass that connects
// the same servers for many agents. After a server fails to connect, later
// clients for the same configuration fail at once instead of starting the
// server again. Stop ends the pass: after it, every client connects normally,
// so a later reload or reconnect can still reach a server that recovered.
type StartupFailureMemo struct {
	inner   ClientFactory
	mu      sync.Mutex
	stopped bool
	failed  map[string]error
}

// NewStartupFailureMemo returns a memo over inner.
func NewStartupFailureMemo(inner ClientFactory) *StartupFailureMemo {
	return &StartupFailureMemo{inner: inner, failed: make(map[string]error)}
}

// Stop ends the startup pass and forgets every recorded failure.
func (m *StartupFailureMemo) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	m.failed = nil
}

// NewClient is a ClientFactory.
func (m *StartupFailureMemo) NewClient(cfg mcp.ServerConfig) (RuntimeClient, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return m.inner(cfg)
	}
	key := string(data)
	m.mu.Lock()
	stopped, failure := m.stopped, m.failed[key]
	m.mu.Unlock()
	if stopped {
		return m.inner(cfg)
	}
	if failure != nil {
		return nil, failure
	}
	client, err := m.inner(cfg)
	if err != nil {
		m.record(key, err)
		return nil, err
	}
	return &memoClient{RuntimeClient: client, memo: m, key: key}, nil
}

func (m *StartupFailureMemo) record(key string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopped {
		m.failed[key] = err
	}
}

type memoClient struct {
	RuntimeClient
	memo *StartupFailureMemo
	key  string
}

func (c *memoClient) Connect(ctx context.Context) error {
	err := c.RuntimeClient.Connect(ctx)
	if err != nil {
		c.memo.record(c.key, err)
	}
	return err
}
