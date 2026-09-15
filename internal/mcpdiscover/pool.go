package mcpdiscover

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/spawn08/chronos/engine/mcp"
)

// SharedClientFactory owns config-keyed transports, not tool registries or
// permissions. Pass the same NewClient method to each Runtime, including late
// connections. Every returned client is a lease and must be closed.
//
// Construction is lazy: the underlying factory runs during Connect. Successful
// connect/list metadata is shared until the last lease closes. Failed startup
// attempts are closed and may be retried; tool calls are never replayed.
type SharedClientFactory struct {
	mu      sync.Mutex
	factory ClientFactory
	entries map[[sha256.Size]byte]*sharedClientEntry
}

type sharedClientEntry struct {
	// refs is protected by the factory mutex. All transport and metadata access
	// is serialized by gate, including shutdown. Neither config nor key is logged.
	refs   int
	config []byte
	gate   chan struct{}
	done   chan struct{}
	prior  <-chan struct{}
	client RuntimeClient
	tools  []byte
}

type sharedClientLease struct {
	pool      *SharedClientFactory
	entry     *sharedClientEntry
	key       [sha256.Size]byte
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

// NewSharedClientFactory uses NewClient when factory is nil. The supplied
// client's context-aware operations must honor cancellation; Close waits for
// an active operation to exit before touching the underlying client.
func NewSharedClientFactory(factory ClientFactory) *SharedClientFactory {
	if factory == nil {
		factory = NewClient
	}
	return &SharedClientFactory{factory: factory, entries: make(map[[sha256.Size]byte]*sharedClientEntry)}
}

// NewClient snapshots the entire configuration, including permission and any
// credentials in arguments/URLs. Only equivalent full configurations share an
// entry. Config bytes and their digest are private and never used in errors.
func (p *SharedClientFactory) NewClient(cfg mcp.ServerConfig) (RuntimeClient, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode shared MCP configuration: %w", err)
	}
	key := sha256.Sum256(data)
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[key]
	if entry == nil || entry.refs == 0 {
		var prior <-chan struct{}
		if entry != nil {
			// A new generation may be leased during shutdown, but must not
			// start a replacement process until that shutdown has finished.
			prior = entry.done
		}
		entry = &sharedClientEntry{
			config: data, gate: make(chan struct{}, 1), done: make(chan struct{}), prior: prior,
		}
		p.entries[key] = entry
	}
	entry.refs++
	ctx, cancel := context.WithCancel(context.Background())
	return &sharedClientLease{pool: p, entry: entry, key: key, ctx: ctx, cancel: cancel}, nil
}

// lock makes both queueing and active IO cancellable by the caller or this
// lease's release. Canceling one waiter does not cancel another lease's IO.
func (l *sharedClientLease) lock(ctx context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	cleanup := func() { stop(); cancel() }
	if l.ctx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, nil, err
	}
	if l.entry.prior != nil {
		select {
		case <-l.entry.prior:
		case <-ctx.Done():
			cleanup()
			return nil, nil, ctx.Err()
		}
	}
	select {
	case l.entry.gate <- struct{}{}:
		unlock := func() { <-l.entry.gate; cleanup() }
		// AfterFunc cancellation is asynchronous. A released waiter must not
		// recreate a transport after final shutdown won the gate.
		if l.ctx.Err() != nil {
			cancel()
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, nil, err
		}
		return ctx, unlock, nil
	case <-ctx.Done():
		cleanup()
		return nil, nil, ctx.Err()
	}
}

func (l *sharedClientLease) Connect(ctx context.Context) error {
	ctx, unlock, err := l.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return l.connectLocked(ctx)
}

func (l *sharedClientLease) connectLocked(ctx context.Context) error {
	e := l.entry
	if e.client != nil {
		return nil
	}
	var cfg mcp.ServerConfig
	if err := json.Unmarshal(e.config, &cfg); err != nil {
		return fmt.Errorf("decode shared MCP configuration: %w", err)
	}
	client, err := l.pool.factory(cfg)
	if err == nil && client == nil {
		err = fmt.Errorf("shared MCP factory returned no client")
	}
	e.client = client
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = client.Connect(ctx)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return errors.Join(fmt.Errorf("connect shared MCP client: %w", err), e.resetLocked())
	}
	return nil
}

func (l *sharedClientLease) ListTools(ctx context.Context) ([]mcp.ToolInfo, error) {
	ctx, unlock, err := l.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := l.connectLocked(ctx); err != nil {
		return nil, err
	}
	e := l.entry
	if e.tools == nil {
		tools, err := e.client.ListTools(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			e.tools, err = json.Marshal(tools)
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("list shared MCP tools: %w", err), e.resetLocked())
		}
	}
	// Each registry gets its own deep copy, including nested JSON schemas.
	var tools []mcp.ToolInfo
	if err := json.Unmarshal(e.tools, &tools); err != nil {
		return nil, fmt.Errorf("decode shared MCP tools: %w", err)
	}
	return tools, nil
}

func (l *sharedClientLease) CallTool(ctx context.Context, name string, args map[string]any) (any, error) {
	ctx, unlock, err := l.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if l.entry.client == nil {
		return nil, fmt.Errorf("shared MCP client is not connected")
	}
	result, err := l.entry.client.CallTool(ctx, name, args)
	if err == nil {
		err = ctx.Err()
	}
	return result, err
}

func (e *sharedClientEntry) resetLocked() error {
	client := e.client
	e.client, e.tools = nil, nil
	if client != nil {
		return client.Close()
	}
	return nil
}

func (l *sharedClientLease) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		p, e := l.pool, l.entry
		p.mu.Lock()
		e.refs--
		last := e.refs == 0
		p.mu.Unlock()
		if !last {
			return
		}
		// Preserve ordering even for an unused generation leased while its
		// predecessor was closing. No pool-wide mutex is held during IO.
		if e.prior != nil {
			<-e.prior
		}
		e.gate <- struct{}{}
		l.closeErr = e.resetLocked()
		close(e.done)
		<-e.gate
		p.mu.Lock()
		if p.entries[l.key] == e {
			delete(p.entries, l.key)
		}
		p.mu.Unlock()
	})
	return l.closeErr
}
