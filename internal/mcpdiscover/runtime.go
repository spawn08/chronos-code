package mcpdiscover

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/security"
)

const DefaultConnectTimeout = 10 * time.Second

type RuntimeClient interface {
	Connect(context.Context) error
	ListTools(context.Context) ([]mcp.ToolInfo, error)
	CallTool(context.Context, string, map[string]any) (any, error)
	Close() error
}

type ClientFactory func(mcp.ServerConfig) (RuntimeClient, error)

type ServerState string

const (
	StateConnected        ServerState = "connected"
	StateDenied           ServerState = "denied"
	StateApprovalRequired ServerState = "approval_required"
	StateInvalid          ServerState = "invalid"
	StateLimitReached     ServerState = "connection_limit_reached"
	StateConnectFailed    ServerState = "connection_failed"
	StateToolsFailed      ServerState = "tool_registration_failed"
)

// ServerStatus excludes commands, arguments, URLs, and raw errors so startup
// diagnostics cannot disclose credentials.
type ServerStatus struct {
	Name  string
	State ServerState
	Tools int
}

type Runtime struct {
	mu        sync.Mutex
	servers   []mcp.ServerConfig
	clients   []RuntimeClient
	statuses  []ServerStatus
	connected int
	closeOnce sync.Once
	closeErr  error
}

func (r *Runtime) Statuses() []ServerStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ServerStatus(nil), r.statuses...)
}

// Server returns the stored definition for name, if the runtime knows it.
func (r *Runtime) Server(name string) (mcp.ServerConfig, bool) {
	if r == nil {
		return mcp.ServerConfig{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serverLocked(name)
}

func (r *Runtime) serverLocked(name string) (mcp.ServerConfig, bool) {
	for _, cfg := range r.servers {
		if cfg.Name == name {
			return cloneServerConfig(cfg), true
		}
	}
	return mcp.ServerConfig{}, false
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		clients := append([]RuntimeClient(nil), r.clients...)
		r.mu.Unlock()
		var errs []error
		for _, client := range clients {
			if err := client.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// Start merges configured and discovered definitions, applies policy before
// client creation, and independently connects each allowed server. Configured
// definitions take precedence by name.
func Start(ctx context.Context, configured, discovered []mcp.ServerConfig, registry *tool.Registry, policy *security.Policy, timeout time.Duration, factory ClientFactory) *Runtime {
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	if factory == nil {
		factory = NewClient
	}
	runtime := &Runtime{servers: mergeServerConfigs(configured, discovered)}
	for _, cfg := range runtime.servers {
		runtime.connectLocked(ctx, cfg, registry, policy, timeout, factory)
	}
	return runtime
}

// RememberServer records a definition so a later ConnectServer call can find it.
func (r *Runtime) RememberServer(cfg mcp.ServerConfig) {
	if r == nil || cfg.Name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.serverLocked(cfg.Name); ok {
		return
	}
	r.servers = append(r.servers, cloneServerConfig(cfg))
	sort.Slice(r.servers, func(i, j int) bool { return r.servers[i].Name < r.servers[j].Name })
}

// ConnectServer attempts to connect one server after session approval. It is a
// no-op when the server is already connected.
func (r *Runtime) ConnectServer(ctx context.Context, cfg mcp.ServerConfig, registry *tool.Registry, policy *security.Policy, timeout time.Duration, factory ClientFactory) ServerStatus {
	if r == nil {
		return ServerStatus{Name: cfg.Name, State: StateConnectFailed}
	}
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	if factory == nil {
		factory = NewClient
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if stored, ok := r.serverLocked(cfg.Name); ok && cfg.Command == "" && cfg.URL == "" {
		cfg = stored
	} else {
		r.replaceServerLocked(cfg)
	}
	return r.connectLocked(ctx, cfg, registry, policy, timeout, factory)
}

func (r *Runtime) replaceServerLocked(cfg mcp.ServerConfig) {
	for i, existing := range r.servers {
		if existing.Name == cfg.Name {
			r.servers[i] = cloneServerConfig(cfg)
			return
		}
	}
	r.servers = append(r.servers, cloneServerConfig(cfg))
	sort.Slice(r.servers, func(i, j int) bool { return r.servers[i].Name < r.servers[j].Name })
}

func (r *Runtime) connectLocked(ctx context.Context, cfg mcp.ServerConfig, registry *tool.Registry, policy *security.Policy, timeout time.Duration, factory ClientFactory) ServerStatus {
	if status, ok := r.statusLocked(cfg.Name); ok && status.State == StateConnected {
		return status
	}

	status := ServerStatus{Name: cfg.Name, State: StateApprovalRequired}
	if policy == nil {
		return r.setStatusLocked(status)
	}
	decision := policy.DecideMCPServer(cfg.Name)
	if decision.Permission == security.MCPDeny {
		status.State = StateDenied
		return r.setStatusLocked(status)
	}
	if decision.Permission != security.MCPAllow {
		status.State = StateApprovalRequired
		return r.setStatusLocked(status)
	}
	if policy.MaxMCPConnections > 0 && r.connected >= policy.MaxMCPConnections {
		status.State = StateLimitReached
		return r.setStatusLocked(status)
	}
	if validateRuntimeConfig(cfg) != nil {
		status.State = StateInvalid
		return r.setStatusLocked(status)
	}

	// Server trust permits startup, not unreviewed tool execution.
	cfg.Permission = string(tool.PermRequireApproval)
	client, err := factory(cfg)
	if err != nil {
		status.State = StateConnectFailed
		return r.setStatusLocked(status)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	err = client.Connect(callCtx)
	cancel()
	if err != nil {
		_ = client.Close()
		status.State = StateConnectFailed
		return r.setStatusLocked(status)
	}

	callCtx, cancel = context.WithTimeout(ctx, timeout)
	tools, err := client.ListTools(callCtx)
	cancel()
	if err != nil || !registerTools(registry, cfg.Name, client, tools) {
		_ = client.Close()
		status.State = StateToolsFailed
		return r.setStatusLocked(status)
	}
	r.clients = append(r.clients, client)
	r.connected++
	status.State = StateConnected
	status.Tools = len(tools)
	return r.setStatusLocked(status)
}

func (r *Runtime) statusLocked(name string) (ServerStatus, bool) {
	for _, status := range r.statuses {
		if status.Name == name {
			return status, true
		}
	}
	return ServerStatus{}, false
}

func (r *Runtime) setStatusLocked(status ServerStatus) ServerStatus {
	for i, existing := range r.statuses {
		if existing.Name == status.Name {
			r.statuses[i] = status
			return status
		}
	}
	r.statuses = append(r.statuses, status)
	sort.Slice(r.statuses, func(i, j int) bool { return r.statuses[i].Name < r.statuses[j].Name })
	return status
}

func mergeServerConfigs(configured, discovered []mcp.ServerConfig) []mcp.ServerConfig {
	byName := make(map[string]mcp.ServerConfig, len(configured)+len(discovered))
	for _, configs := range [][]mcp.ServerConfig{configured, discovered} {
		for _, cfg := range configs {
			if _, exists := byName[cfg.Name]; !exists {
				byName[cfg.Name] = cfg
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]mcp.ServerConfig, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func cloneServerConfig(cfg mcp.ServerConfig) mcp.ServerConfig {
	cfg.Args = append([]string(nil), cfg.Args...)
	return cfg
}

func validateRuntimeConfig(cfg mcp.ServerConfig) error {
	return ValidateManagedServer(ManagedServer{
		Name: cfg.Name, Transport: cfg.Transport, Command: cfg.Command,
		Args: cfg.Args, URL: cfg.URL, Permission: cfg.Permission,
	})
}

func registerTools(registry *tool.Registry, server string, client RuntimeClient, tools []mcp.ToolInfo) bool {
	if registry == nil {
		return false
	}
	existing := make(map[string]struct{})
	for _, definition := range registry.List() {
		existing[definition.Name] = struct{}{}
	}
	type pendingTool struct {
		name string
		info mcp.ToolInfo
	}
	pending := make([]pendingTool, 0, len(tools))
	for _, info := range tools {
		name := ToolName(server, info.Name)
		if _, collision := existing[name]; collision || info.Name == "" {
			return false
		}
		existing[name] = struct{}{}
		pending = append(pending, pendingTool{name: name, info: info})
	}
	for _, item := range pending {
		remoteName := item.info.Name
		registry.Register(&tool.Definition{
			Name: item.name, Description: item.info.Description,
			Parameters: item.info.InputSchema, Permission: tool.PermRequireApproval,
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				return client.CallTool(ctx, remoteName, args)
			},
		})
	}
	return true
}

func ToolName(server, remote string) string {
	return "mcp__" + safeToolPart(server) + "__" + safeToolPart(remote)
}

func safeToolPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func NewClient(cfg mcp.ServerConfig) (RuntimeClient, error) {
	return mcp.NewClient(cfg)
}
