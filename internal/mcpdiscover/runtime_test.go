package mcpdiscover

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/security"
)

type fakeRuntimeClient struct {
	connectErr error
	toolsErr   error
	tools      []mcp.ToolInfo
	closed     int
	calls      []string
}

func (c *fakeRuntimeClient) Connect(context.Context) error { return c.connectErr }
func (c *fakeRuntimeClient) ListTools(context.Context) ([]mcp.ToolInfo, error) {
	return c.tools, c.toolsErr
}
func (c *fakeRuntimeClient) CallTool(_ context.Context, name string, _ map[string]any) (any, error) {
	c.calls = append(c.calls, name)
	return name, nil
}
func (c *fakeRuntimeClient) Close() error { c.closed++; return nil }

func TestStartDoesNotCreateDeniedUntrustedOrInvalidServers(t *testing.T) {
	registry := tool.NewRegistry()
	policy := &security.Policy{
		DeniedMCPServers:     []string{"denied"},
		TrustedMCPServers:    []string{"invalid"},
		MCPDefaultPermission: security.MCPRequireApproval,
	}
	created := 0
	runtime := Start(context.Background(), []mcp.ServerConfig{
		{Name: "invalid", Transport: mcp.Transport("http"), URL: "https://example.test"},
	}, []mcp.ServerConfig{
		{Name: "denied", Transport: mcp.TransportStdio, Command: "must-not-run"},
		{Name: "untrusted", Transport: mcp.TransportStdio, Command: "must-not-run"},
	}, registry, policy, time.Second, func(mcp.ServerConfig) (RuntimeClient, error) {
		created++
		return &fakeRuntimeClient{}, nil
	})
	if created != 0 {
		t.Fatalf("created clients = %d, want 0", created)
	}
	statuses := runtime.Statuses()
	if len(statuses) != 3 || statuses[0].State != StateDenied || statuses[1].State != StateInvalid || statuses[2].State != StateApprovalRequired {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestStartDoesNotCreateTrustedServerWithPlaintextCredentials(t *testing.T) {
	policy := &security.Policy{TrustedMCPServers: []string{"stdio", "sse"}, MCPDefaultPermission: security.MCPRequireApproval}
	created := 0
	runtime := Start(context.Background(), []mcp.ServerConfig{
		{Name: "stdio", Transport: mcp.TransportStdio, Command: "server", Args: []string{"--api-key=plaintext"}},
		{Name: "sse", Transport: mcp.TransportSSE, URL: "https://mcp.example.test/events?token=plaintext"},
	}, nil, tool.NewRegistry(), policy, time.Second, func(mcp.ServerConfig) (RuntimeClient, error) {
		created++
		return &fakeRuntimeClient{}, nil
	})
	if created != 0 {
		t.Fatalf("created clients = %d, want 0", created)
	}
	for _, status := range runtime.Statuses() {
		if status.State != StateInvalid {
			t.Fatalf("statuses = %#v, want invalid credentials", runtime.Statuses())
		}
	}
}

func TestStartDoesNotTrustDiscoveredDefinitionByName(t *testing.T) {
	policy := &security.Policy{TrustedMCPServers: []string{"filesystem"}, MCPDefaultPermission: security.MCPRequireApproval}
	created := 0
	runtime := Start(context.Background(), nil, []mcp.ServerConfig{
		{Name: "filesystem", Transport: mcp.TransportStdio, Command: "replacement"},
	}, tool.NewRegistry(), policy, time.Second, func(mcp.ServerConfig) (RuntimeClient, error) {
		created++
		return &fakeRuntimeClient{}, nil
	})
	if created != 0 {
		t.Fatalf("created clients = %d, want 0", created)
	}
	if statuses := runtime.Statuses(); len(statuses) != 1 || statuses[0].State != StateApprovalRequired {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestStartNamespacesToolsWithoutReplacingNativeTools(t *testing.T) {
	registry := tool.NewRegistry()
	nativeCalls := 0
	registry.Register(&tool.Definition{Name: "shell", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
		nativeCalls++
		return "native", nil
	}})
	client := &fakeRuntimeClient{tools: []mcp.ToolInfo{{Name: "shell"}}}
	runtime := Start(context.Background(), []mcp.ServerConfig{{Name: "filesystem", Transport: mcp.TransportStdio, Command: "server"}}, nil, registry,
		&security.Policy{TrustedMCPServers: []string{"filesystem"}, MCPDefaultPermission: security.MCPRequireApproval}, time.Second,
		func(cfg mcp.ServerConfig) (RuntimeClient, error) {
			if cfg.Permission != string(tool.PermRequireApproval) {
				t.Fatalf("runtime permission = %q", cfg.Permission)
			}
			return client, nil
		})
	if statuses := runtime.Statuses(); len(statuses) != 1 || statuses[0].State != StateConnected {
		t.Fatalf("statuses = %#v", statuses)
	}
	definitions := make(map[string]*tool.Definition)
	for _, definition := range registry.List() {
		definitions[definition.Name] = definition
	}
	name := ToolName("filesystem", "shell")
	if definitions["shell"] == nil || definitions[name] == nil {
		t.Fatalf("registered tools = %#v", definitions)
	}
	if definitions[name].Permission != tool.PermRequireApproval {
		t.Fatalf("MCP permission = %q", definitions[name].Permission)
	}
	if _, err := definitions["shell"].Handler(context.Background(), nil); err != nil || nativeCalls != 1 {
		t.Fatalf("native tool replaced: calls=%d err=%v", nativeCalls, err)
	}
	if _, err := definitions[name].Handler(context.Background(), nil); err != nil || len(client.calls) != 1 || client.calls[0] != "shell" {
		t.Fatalf("MCP routing = %v, %v", client.calls, err)
	}
}

func TestStartIsolatesFailuresAndClosesEveryCreatedClientOnce(t *testing.T) {
	registry := tool.NewRegistry()
	clients := map[string]*fakeRuntimeClient{
		"bad-connect": {connectErr: errors.New("secret endpoint failure")},
		"bad-tools":   {toolsErr: errors.New("secret tool failure")},
		"healthy":     {tools: []mcp.ToolInfo{{Name: "read"}}},
	}
	policy := &security.Policy{TrustedMCPServers: []string{"bad-connect", "bad-tools", "healthy"}, MCPDefaultPermission: security.MCPRequireApproval}
	runtime := Start(context.Background(), []mcp.ServerConfig{
		{Name: "healthy", Transport: mcp.TransportStdio, Command: "server"},
		{Name: "bad-tools", Transport: mcp.TransportStdio, Command: "server"},
		{Name: "bad-connect", Transport: mcp.TransportStdio, Command: "server"},
	}, nil, registry, policy, time.Second, func(cfg mcp.ServerConfig) (RuntimeClient, error) { return clients[cfg.Name], nil })
	statuses := runtime.Statuses()
	if len(statuses) != 3 || statuses[0].State != StateConnectFailed || statuses[1].State != StateToolsFailed || statuses[2].State != StateConnected {
		t.Fatalf("statuses = %#v", statuses)
	}
	if _, err := registry.Execute(context.Background(), ToolName("healthy", "read"), nil); err == nil {
		t.Fatal("MCP tool executed without approval")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, ok := registry.Get(ToolName("healthy", "read")); ok {
		t.Fatal("Close retained an MCP namespace")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	for name, client := range clients {
		if client.closed != 1 {
			t.Errorf("%s close count = %d, want 1", name, client.closed)
		}
	}
}

func TestStartConfiguredDefinitionWinsAndClientsAreIndependent(t *testing.T) {
	configured := []mcp.ServerConfig{{Name: "filesystem", Transport: mcp.TransportStdio, Command: "configured"}}
	discovered := []mcp.ServerConfig{{Name: "filesystem", Transport: mcp.TransportStdio, Command: "discovered"}}
	policy := &security.Policy{TrustedMCPServers: []string{"filesystem"}, MCPDefaultPermission: security.MCPRequireApproval}
	var clients []*fakeRuntimeClient
	factory := func(cfg mcp.ServerConfig) (RuntimeClient, error) {
		if cfg.Command != "configured" {
			t.Fatalf("merged command = %q, want configured", cfg.Command)
		}
		client := &fakeRuntimeClient{}
		clients = append(clients, client)
		return client, nil
	}
	runtimeA := Start(context.Background(), configured, discovered, tool.NewRegistry(), policy, time.Second, factory)
	runtimeB := Start(context.Background(), configured, discovered, tool.NewRegistry(), policy, time.Second, factory)
	if len(clients) != 2 || clients[0] == clients[1] {
		t.Fatalf("clients = %#v, want one independent client per agent", clients)
	}
	_ = runtimeA.Close()
	_ = runtimeB.Close()
}

func TestConnectServerApprovesPreviouslyPendingServer(t *testing.T) {
	registry := tool.NewRegistry()
	policy := &security.Policy{MCPDefaultPermission: security.MCPRequireApproval}
	client := &fakeRuntimeClient{tools: []mcp.ToolInfo{{Name: "read"}}}
	cfg := mcp.ServerConfig{Name: "filesystem", Transport: mcp.TransportStdio, Command: "server"}
	runtime := Start(context.Background(), nil, []mcp.ServerConfig{cfg}, registry, policy, time.Second, func(mcp.ServerConfig) (RuntimeClient, error) {
		t.Fatal("untrusted server created a client")
		return client, nil
	})
	if statuses := runtime.Statuses(); len(statuses) != 1 || statuses[0].State != StateApprovalRequired {
		t.Fatalf("statuses = %#v", runtime.Statuses())
	}
	if err := policy.AllowMCPServerSession("filesystem"); err != nil {
		t.Fatal(err)
	}
	status := runtime.ConnectServer(context.Background(), cfg, registry, policy, time.Second, func(mcp.ServerConfig) (RuntimeClient, error) {
		return client, nil
	})
	if status.State != StateConnected || status.Tools != 1 {
		t.Fatalf("status = %#v", status)
	}
	if _, err := registry.Execute(context.Background(), ToolName("filesystem", "read"), nil); err == nil {
		t.Fatal("MCP tool executed without approval")
	}
}

func TestSessionApprovalDoesNotTrustChangedLaunchConfig(t *testing.T) {
	registry := tool.NewRegistry()
	policy := &security.Policy{MCPDefaultPermission: security.MCPRequireApproval}
	initial := mcp.ServerConfig{Name: "filesystem", Transport: mcp.TransportStdio, Command: "v1"}
	created := 0
	factory := func(mcp.ServerConfig) (RuntimeClient, error) {
		created++
		return &fakeRuntimeClient{tools: []mcp.ToolInfo{{Name: "read"}}}, nil
	}
	runtime := Start(context.Background(), nil, []mcp.ServerConfig{initial}, registry, policy, time.Second, factory)
	if err := policy.AllowMCPServerSession("filesystem"); err != nil {
		t.Fatal(err)
	}
	if status := runtime.ConnectServer(context.Background(), initial, registry, policy, time.Second, factory); status.State != StateConnected {
		t.Fatalf("approved status = %#v", status)
	}

	changed := initial
	changed.Command = "v2"
	statuses := runtime.ReloadDiscovery(context.Background(), Snapshot{Servers: []mcp.ServerConfig{changed}})
	if len(statuses) != 1 || statuses[0].State != StateReloadFailed || !statuses[0].Retained {
		t.Fatalf("changed status = %#v", statuses)
	}
	if created != 1 {
		t.Fatalf("created clients = %d, want 1", created)
	}
}

func TestReloadDiscoveryAtomicallyReplacesRemovesAndRetainsOnFailure(t *testing.T) {
	registry := tool.NewRegistry()
	policy := &security.Policy{MCPDefaultPermission: security.MCPAllow}
	oldClient := &fakeRuntimeClient{tools: []mcp.ToolInfo{{Name: "old"}}}
	newClient := &fakeRuntimeClient{tools: []mcp.ToolInfo{{Name: "new"}}}
	failingClient := &fakeRuntimeClient{connectErr: errors.New("unavailable")}
	clients := []RuntimeClient{oldClient, failingClient, failingClient, failingClient, newClient}
	factory := func(mcp.ServerConfig) (RuntimeClient, error) {
		client := clients[0]
		clients = clients[1:]
		return client, nil
	}
	initial := mcp.ServerConfig{Name: "filesystem", Transport: mcp.TransportStdio, Command: "v1", Permission: "require_approval"}
	runtime := Start(context.Background(), nil, []mcp.ServerConfig{initial}, registry, policy, time.Second, factory)
	runtime.SetAgent("coder")
	runtime.SetDiscoveryMetadata(Snapshot{
		Servers: []mcp.ServerConfig{initial},
		Sources: []SourceStatus{{Path: "/project/.mcp.json", State: SourceHealthy, Servers: []mcp.ServerConfig{initial}}},
	}, nil)

	failed := initial
	failed.Command = "broken"
	statuses := runtime.ReloadDiscovery(context.Background(), Snapshot{
		Servers: []mcp.ServerConfig{failed},
		Sources: []SourceStatus{{Path: "/project/.mcp.json", State: SourceHealthy, Servers: []mcp.ServerConfig{failed}}},
	})
	if statuses[0].State != StateReloadFailed || !statuses[0].Retained || statuses[0].Agent != "coder" || statuses[0].Source != "/project/.mcp.json" {
		t.Fatalf("failed reload status = %+v", statuses)
	}
	if _, ok := registry.Get(ToolName("filesystem", "old")); !ok {
		t.Fatal("failed reload removed healthy namespace")
	}

	replacement := initial
	replacement.Command = "v2"
	runtime.ReloadDiscovery(context.Background(), Snapshot{
		Servers: []mcp.ServerConfig{replacement},
		Sources: []SourceStatus{{Path: "/project/.mcp.json", State: SourceHealthy, Servers: []mcp.ServerConfig{replacement}}},
	})
	if _, ok := registry.Get(ToolName("filesystem", "old")); ok {
		t.Fatal("successful reload retained old tool")
	}
	if _, ok := registry.Get(ToolName("filesystem", "new")); !ok {
		t.Fatal("successful reload did not install replacement tool")
	}
	runtime.ReloadDiscovery(context.Background(), Snapshot{})
	if _, ok := registry.Get(ToolName("filesystem", "new")); ok {
		t.Fatal("removed server retained namespace")
	}
}
