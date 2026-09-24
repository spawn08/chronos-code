package mcpdiscover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/security"
)

type poolTestClient struct {
	connects, lists, calls, closes atomic.Int32
	connect                        func(context.Context) error
	list                           func(context.Context) ([]mcp.ToolInfo, error)
	call                           func(context.Context) (any, error)
	close                          func() error
}

func (c *poolTestClient) Connect(ctx context.Context) error {
	c.connects.Add(1)
	if c.connect != nil {
		return c.connect(ctx)
	}
	return nil
}

func (c *poolTestClient) ListTools(ctx context.Context) ([]mcp.ToolInfo, error) {
	c.lists.Add(1)
	if c.list != nil {
		return c.list(ctx)
	}
	return []mcp.ToolInfo{{Name: "read", InputSchema: map[string]any{
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
	}}}, nil
}

func (c *poolTestClient) CallTool(ctx context.Context, _ string, _ map[string]any) (any, error) {
	c.calls.Add(1)
	if c.call != nil {
		return c.call(ctx)
	}
	return "ok", nil
}

func (c *poolTestClient) Close() error {
	c.closes.Add(1)
	if c.close != nil {
		return c.close()
	}
	return nil
}

func poolConfig() mcp.ServerConfig {
	return mcp.ServerConfig{Name: "filesystem", Transport: mcp.TransportStdio, Command: "server"}
}

func poolLease(t *testing.T, pool *SharedClientFactory, cfg mcp.ServerConfig) RuntimeClient {
	t.Helper()
	client, err := pool.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func poolReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pool operation")
		var zero T
		return zero
	}
}

func TestSharedPoolRuntimesShareTransportAndKeepRegistryApproval(t *testing.T) {
	const n = 24
	backend := &poolTestClient{}
	var created atomic.Int32
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
		created.Add(1)
		return backend, nil
	})
	policy := &security.Policy{TrustedMCPServers: []string{"filesystem"}}
	runtimes := make([]*Runtime, n)
	registries := make([]*tool.Registry, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			registries[i] = tool.NewRegistry()
			runtimes[i] = Start(context.Background(), []mcp.ServerConfig{poolConfig()}, nil, registries[i], policy, time.Second, pool.NewClient)
		})
	}
	wg.Wait()
	for _, runtime := range runtimes {
		t.Cleanup(func() { _ = runtime.Close() })
		if got := runtime.Statuses(); len(got) != 1 || got[0].State != StateConnected {
			t.Fatalf("statuses = %v", got)
		}
	}
	if created.Load() != 1 || backend.connects.Load() != 1 || backend.lists.Load() != 1 {
		t.Fatalf("create/connect/list = %d/%d/%d", created.Load(), backend.connects.Load(), backend.lists.Load())
	}
	name := ToolName("filesystem", "read")
	registries[0].SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) { return true, nil })
	if _, err := registries[0].Execute(context.Background(), name, nil); err != nil {
		t.Fatal(err)
	}
	for _, registry := range registries[1:] {
		if _, err := registry.Execute(context.Background(), name, nil); err == nil {
			t.Fatal("another registry inherited approval")
		}
	}
	// Registration metadata must not alias across agents, even at nested levels.
	first := registries[0].List()[0]
	first.Parameters["properties"].(map[string]any)["path"].(map[string]any)["type"] = "number"
	if got := registries[1].List()[0].Parameters["properties"].(map[string]any)["path"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("shared mutable schema: %v", got)
	}
	for _, runtime := range runtimes[:n-1] {
		wg.Go(func() { _ = runtime.Close(); _ = runtime.Close() })
	}
	wg.Wait()
	if backend.closes.Load() != 0 {
		t.Fatal("closed transport with a live runtime")
	}
	if err := runtimes[n-1].Close(); err != nil {
		t.Fatal(err)
	}
	if backend.closes.Load() != 1 || backend.calls.Load() != 1 {
		t.Fatalf("closes/calls = %d/%d", backend.closes.Load(), backend.calls.Load())
	}
}

func TestSharedPoolFullConfigIsolationAndSnapshot(t *testing.T) {
	base := poolConfig()
	base.Args = []string{"--token", "private-one"}
	base.URL = "https://example.test/events?token=private-one"
	base.Permission = "require_approval"
	variants := []func(*mcp.ServerConfig){
		func(c *mcp.ServerConfig) { c.Name = "other" },
		func(c *mcp.ServerConfig) { c.Transport = mcp.TransportSSE },
		func(c *mcp.ServerConfig) { c.Command = "other" },
		func(c *mcp.ServerConfig) { c.Args = []string{"--token", "private-two"} },
		func(c *mcp.ServerConfig) { c.URL = "https://example.test/events?token=private-two" },
		func(c *mcp.ServerConfig) { c.Permission = "deny" },
	}
	var configs []mcp.ServerConfig
	pool := NewSharedClientFactory(func(cfg mcp.ServerConfig) (RuntimeClient, error) {
		configs = append(configs, cfg)
		return &poolTestClient{}, nil
	})
	leases := []RuntimeClient{poolLease(t, pool, base), poolLease(t, pool, base)}
	for _, change := range variants {
		cfg := base
		change(&cfg)
		leases = append(leases, poolLease(t, pool, cfg))
	}
	base.Args[1] = "mutated"
	for _, lease := range leases {
		if err := lease.Connect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(configs) != 1+len(variants) {
		t.Fatalf("created %d distinct configs", len(configs))
	}
	if configs[0].Args[1] != "private-one" {
		t.Fatal("factory used caller-mutated config")
	}
}

func TestSharedPoolInitializationFailuresRetry(t *testing.T) {
	failure := errors.New("initialization failed")
	for _, stage := range []string{"factory", "nil_client", "connect", "list", "schema"} {
		t.Run(stage, func(t *testing.T) {
			bad, good := &poolTestClient{}, &poolTestClient{}
			switch stage {
			case "connect":
				bad.connect = func(context.Context) error { return failure }
			case "list":
				bad.list = func(context.Context) ([]mcp.ToolInfo, error) { return nil, failure }
			case "schema":
				bad.list = func(context.Context) ([]mcp.ToolInfo, error) {
					return []mcp.ToolInfo{{Name: "read", InputSchema: map[string]any{"invalid": make(chan int)}}}, nil
				}
			}
			created := 0
			pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
				created++
				if created == 1 {
					if stage == "factory" {
						return bad, failure
					}
					if stage == "nil_client" {
						return nil, nil
					}
					return bad, nil
				}
				return good, nil
			})
			first, second := poolLease(t, pool, poolConfig()), poolLease(t, pool, poolConfig())
			err := first.Connect(context.Background())
			if stage == "list" || stage == "schema" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = first.ListTools(context.Background())
			}
			if err == nil {
				t.Fatal("initialization unexpectedly succeeded")
			}
			if err := second.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := second.ListTools(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = first.Close()
			if good.closes.Load() != 0 {
				t.Fatal("failed lease release closed healthy replacement")
			}
			_ = second.Close()
			wantBadCloses := int32(1)
			if stage == "nil_client" {
				wantBadCloses = 0
			}
			if created != 2 || bad.closes.Load() != wantBadCloses || good.closes.Load() != 1 {
				t.Fatalf("created/bad closes/good closes = %d/%d/%d", created, bad.closes.Load(), good.closes.Load())
			}
		})
	}
}

func TestSharedPoolConnectWaitCancellationDoesNotCancelOwner(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	backend := &poolTestClient{connect: func(ctx context.Context) error {
		close(entered)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) { return backend, nil })
	first, second := poolLease(t, pool, poolConfig()), poolLease(t, pool, poolConfig())
	done := make(chan error, 1)
	go func() { done <- first.Connect(context.Background()) }()
	poolReceive(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- second.Connect(ctx) }()
	cancel()
	if err := poolReceive(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	_ = second.Close()
	close(finish)
	if err := poolReceive(t, done); err != nil {
		t.Fatalf("owner was canceled: %v", err)
	}
	if backend.connects.Load() != 1 || backend.closes.Load() != 0 {
		t.Fatal("canceled waiter changed the transport lifecycle")
	}
}

func TestSharedPoolReleaseCancelsConnectAndSurvivorRetries(t *testing.T) {
	entered := make(chan struct{})
	bad := &poolTestClient{connect: func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	good := &poolTestClient{}
	var created atomic.Int32
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
		if created.Add(1) == 1 {
			return bad, nil
		}
		return good, nil
	})
	first, second := poolLease(t, pool, poolConfig()), poolLease(t, pool, poolConfig())
	done := make(chan error, 1)
	go func() { done <- first.Connect(context.Background()) }()
	poolReceive(t, entered)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := poolReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("released connect error = %v", err)
	}
	if err := second.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bad.closes.Load() != 1 || good.connects.Load() != 1 {
		t.Fatal("canceled initialization was not replaced")
	}
}

func TestSharedPoolCallsSerializeAndObserveCancellation(t *testing.T) {
	entered := make(chan struct{}, 8)
	backend := &poolTestClient{call: func(ctx context.Context) (any, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) { return backend, nil })
	first, second := poolLease(t, pool, poolConfig()), poolLease(t, pool, poolConfig())
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := first.CallTool(context.Background(), "read", nil); done <- err }()
	poolReceive(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := second.CallTool(ctx, "read", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call error = %v", err)
	}
	if backend.calls.Load() != 1 {
		t.Fatal("queued call entered transport while another call was running")
	}
	_ = first.Close()
	if err := poolReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("released call error = %v", err)
	}
	if backend.closes.Load() != 0 {
		t.Fatal("release closed another lease's transport")
	}
	if _, err := first.CallTool(context.Background(), "read", nil); err == nil {
		t.Fatal("released lease remains usable")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := second.CallTool(ctx, "read", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active call cancellation error = %v", err)
	}
	if backend.calls.Load() != 2 || backend.connects.Load() != 1 {
		t.Fatal("canceled tool call was replayed or reconnected")
	}
}

func TestSharedPoolFinalCloseAndReconnectDoNotOverlap(t *testing.T) {
	closing, finish := make(chan struct{}), make(chan struct{})
	finishClose := sync.OnceFunc(func() { close(finish) })
	closeErr := errors.New("close failed")
	old := &poolTestClient{close: func() error { close(closing); <-finish; return closeErr }}
	fresh := &poolTestClient{}
	var created atomic.Int32
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
		if created.Add(1) == 1 {
			return old, nil
		}
		return fresh, nil
	})
	first := poolLease(t, pool, poolConfig())
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(finishClose)
	done := make(chan error, 1)
	go func() { done <- first.Close() }()
	poolReceive(t, closing)
	second := poolLease(t, pool, poolConfig())
	t.Cleanup(finishClose)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := second.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconnect waiting for close = %v", err)
	}
	if created.Load() != 1 {
		t.Fatal("replacement created before previous transport closed")
	}
	finishClose()
	if err := poolReceive(t, done); !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
	if err := first.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("repeated close error = %v", err)
	}
	if err := second.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	if created.Load() != 2 || old.closes.Load() != 1 || fresh.closes.Load() != 1 || fresh.lists.Load() != 1 {
		t.Fatal("incorrect reconnect lifecycle")
	}
}

func TestSharedPoolLateTrustAndRegistrationFailureAreLocal(t *testing.T) {
	backend := &poolTestClient{}
	created := 0
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) { created++; return backend, nil })
	trusted := &security.Policy{TrustedMCPServers: []string{"filesystem"}}
	pending := &security.Policy{MCPDefaultPermission: security.MCPRequireApproval}
	denied := &security.Policy{DeniedMCPServers: []string{"filesystem"}}
	start := func(registry *tool.Registry, policy *security.Policy) *Runtime {
		r := Start(context.Background(), []mcp.ServerConfig{poolConfig()}, nil, registry, policy, time.Second, pool.NewClient)
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	_ = start(tool.NewRegistry(), trusted)
	lateRegistry := tool.NewRegistry()
	late := start(lateRegistry, pending)
	blockedRegistry := tool.NewRegistry()
	blocked := start(blockedRegistry, denied)
	if created != 1 || len(lateRegistry.List()) != 0 || len(blockedRegistry.List()) != 0 || blocked.Statuses()[0].State != StateDenied {
		t.Fatal("shared transport bypassed server policy")
	}
	identity, ok := late.Identity("filesystem")
	if !ok {
		t.Fatal("server identity unavailable")
	}
	if err := pending.AllowMCPServerSessionIdentity(identity); err != nil {
		t.Fatal(err)
	}
	if status := late.ConnectServer(context.Background(), poolConfig(), lateRegistry, pending, time.Second, pool.NewClient); status.State != StateConnected {
		t.Fatalf("late status = %v", status)
	}
	collisionRegistry := tool.NewRegistry()
	collisionRegistry.Register(&tool.Definition{Name: ToolName("filesystem", "read")})
	collision := start(collisionRegistry, trusted)
	if collision.Statuses()[0].State != StateToolsFailed || backend.closes.Load() != 0 || created != 1 || backend.lists.Load() != 1 {
		t.Fatal("registry collision affected shared transport")
	}
	if _, err := lateRegistry.Execute(context.Background(), ToolName("filesystem", "read"), nil); err == nil {
		t.Fatal("late registration bypassed tool approval")
	}
}

func TestSharedPoolUnusedAndConcurrentLeaseRelease(t *testing.T) {
	var created atomic.Int32
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
		created.Add(1)
		return nil, fmt.Errorf("must not create")
	})
	lease := poolLease(t, pool, poolConfig())
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { _ = lease.Close() })
	}
	wg.Wait()
	if err := lease.Connect(context.Background()); err == nil {
		t.Fatal("connected released lease")
	}
	if created.Load() != 0 {
		t.Fatal("unused lease created a transport")
	}
}

func TestSharedPoolCanceledInitializationRetries(t *testing.T) {
	for _, stage := range []string{"connect", "list"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			wait := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			bad, good := &poolTestClient{}, &poolTestClient{}
			if stage == "connect" {
				bad.connect = wait
			} else {
				bad.list = func(ctx context.Context) ([]mcp.ToolInfo, error) { return nil, wait(ctx) }
			}
			var created atomic.Int32
			pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
				if created.Add(1) == 1 {
					return bad, nil
				}
				return good, nil
			})
			first, second := poolLease(t, pool, poolConfig()), poolLease(t, pool, poolConfig())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				err := first.Connect(ctx)
				if err == nil {
					_, err = first.ListTools(ctx)
				}
				done <- err
			}()
			poolReceive(t, entered)
			cancel()
			if err := poolReceive(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled %s error = %v", stage, err)
			}
			if err := second.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := second.ListTools(context.Background()); err != nil {
				t.Fatal(err)
			}
			if bad.closes.Load() != 1 || good.connects.Load() != 1 || good.lists.Load() != 1 {
				t.Fatal("canceled initialization poisoned replacement")
			}
		})
	}
}

func TestSharedPoolConcurrentConnectAndLastRelease(t *testing.T) {
	var created, closed atomic.Int32
	pool := NewSharedClientFactory(func(mcp.ServerConfig) (RuntimeClient, error) {
		created.Add(1)
		return &poolTestClient{close: func() error { closed.Add(1); return nil }}, nil
	})
	for range 256 {
		lease := poolLease(t, pool, poolConfig())
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() { <-start; _ = lease.Connect(context.Background()) })
		wg.Go(func() { <-start; _ = lease.Close() })
		close(start)
		wg.Wait()
	}
	if created.Load() != closed.Load() {
		t.Fatalf("concurrent connect/release leaked clients: created=%d closed=%d", created.Load(), closed.Load())
	}
}
