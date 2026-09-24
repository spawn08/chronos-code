package mcpdiscover

import (
	"context"
	"errors"
	"reflect"
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

const (
	defaultReloadAttempts = 3
	defaultReloadBackoff  = 100 * time.Millisecond
)

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
	StateReloadFailed     ServerState = "reload_failed"
)

// ServerStatus excludes commands, arguments, URLs, and raw errors so startup
// diagnostics cannot disclose credentials.
type ServerStatus struct {
	Name     string
	Agent    string
	Source   string
	State    ServerState
	Tools    int
	Retained bool
	Attempts int
}

type activeServer struct {
	config    mcp.ServerConfig
	client    RuntimeClient
	toolNames []string
}

type ToolTransform func([]*tool.Definition) []*tool.Definition

type Runtime struct {
	mu         sync.Mutex
	servers    []mcp.ServerConfig
	configured map[string]mcp.ServerConfig
	discovered map[string]mcp.ServerConfig
	sources    map[string]string
	active     map[string]*activeServer
	registry   *tool.Registry
	policy     *security.Policy
	timeout    time.Duration
	factory    ClientFactory
	transform  ToolTransform
	agent      string
	statuses   []ServerStatus
	connected  int
	closed     bool
	closeOnce  sync.Once
	closeErr   error
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
		clients := make([]RuntimeClient, 0, len(r.active))
		var toolNames []string
		for _, active := range r.active {
			clients = append(clients, active.client)
			toolNames = append(toolNames, active.toolNames...)
		}
		if r.registry != nil {
			_ = r.registry.Replace(toolNames, nil)
		}
		r.active = make(map[string]*activeServer)
		r.connected = 0
		r.closed = true
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
	runtime := &Runtime{
		configured: configMap(configured), discovered: configMap(discovered), sources: make(map[string]string),
		active: make(map[string]*activeServer), registry: registry, policy: policy, timeout: timeout, factory: factory,
	}
	runtime.servers = mergeServerConfigs(configured, discovered)
	if policy != nil {
		for _, cfg := range configured {
			policy.TrustConfiguredMCPServer(serverIdentity(cfg, "configured"))
		}
	}
	for _, cfg := range runtime.servers {
		runtime.connectLocked(ctx, cfg, registry, policy, timeout, factory)
	}
	return runtime
}

// SetAgent labels subsequent statuses with the owning agent.
func (r *Runtime) SetAgent(agent string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.agent = agent
	for i := range r.statuses {
		r.statuses[i].Agent = agent
	}
}

// SetDiscoveryMetadata labels discovered servers and configures wrapping for
// definitions prepared by future reloads.
func (r *Runtime) SetDiscoveryMetadata(snapshot Snapshot, transform ToolTransform) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.sources = make(map[string]string, len(snapshot.Servers))
	for _, cfg := range snapshot.Servers {
		r.sources[cfg.Name] = snapshot.SourceForServer(cfg.Name)
	}
	for name := range r.configured {
		r.sources[name] = "agent-config"
	}
	r.transform = transform
	for i := range r.statuses {
		r.statuses[i].Source = r.sources[r.statuses[i].Name]
	}
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
	if r.closed {
		return ServerStatus{Name: cfg.Name, Agent: r.agent, State: StateConnectFailed}
	}
	if stored, ok := r.serverLocked(cfg.Name); ok && cfg.Command == "" && cfg.URL == "" {
		cfg = stored
	} else {
		r.replaceServerLocked(cfg)
	}
	_ = policy.BindMCPServerSession(r.serverIdentityLocked(cfg))
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

	status := ServerStatus{Name: cfg.Name, Agent: r.agent, Source: r.sources[cfg.Name], State: StateApprovalRequired}
	if policy == nil {
		return r.setStatusLocked(status)
	}
	decision := policy.DecideMCPServerIdentity(r.serverIdentityLocked(cfg))
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
	definitions, names, definitionsOK := toolDefinitions(cfg.Name, client, tools)
	if err != nil || !definitionsOK || registry == nil || registry.Replace(nil, definitions) != nil {
		_ = client.Close()
		status.State = StateToolsFailed
		return r.setStatusLocked(status)
	}
	r.active[cfg.Name] = &activeServer{config: cloneServerConfig(cfg), client: client, toolNames: names}
	r.connected++
	status.State = StateConnected
	status.Tools = len(tools)
	return r.setStatusLocked(status)
}

// ReloadDiscovery transactionally applies a discovery snapshot. Each changed
// server is connected and listed before its namespace is atomically replaced;
// failed reloads leave the previous client and tools active. Removed discovery
// servers are unregistered unless shadowed by configured agent MCP settings.
func (r *Runtime) ReloadDiscovery(ctx context.Context, snapshot Snapshot) []ServerStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return append([]ServerStatus(nil), r.statuses...)
	}
	r.discovered = configMap(snapshot.Servers)
	r.sources = make(map[string]string, len(snapshot.Servers))
	for _, cfg := range snapshot.Servers {
		r.sources[cfg.Name] = snapshot.SourceForServer(cfg.Name)
	}
	for name := range r.configured {
		r.sources[name] = "agent-config"
	}
	next := mergeServerConfigs(mapConfigs(r.configured), snapshot.Servers)
	nextByName := configMap(next)

	for name, active := range r.active {
		if _, keep := nextByName[name]; keep {
			continue
		}
		if r.registry != nil && r.registry.Replace(active.toolNames, nil) == nil {
			_ = active.client.Close()
			delete(r.active, name)
			r.connected--
			r.removeStatusLocked(name)
		}
	}
	for _, cfg := range next {
		if active := r.active[cfg.Name]; active != nil && reflect.DeepEqual(active.config, cfg) {
			if status, ok := r.statusLocked(cfg.Name); ok {
				status.Source = r.sources[cfg.Name]
				r.setStatusLocked(status)
			}
			continue
		}
		r.reloadServerLocked(ctx, cfg)
	}
	effective := cloneServers(next)
	for i := range effective {
		if active := r.active[effective[i].Name]; active != nil {
			effective[i] = cloneServerConfig(active.config)
		}
	}
	r.servers = effective
	return append([]ServerStatus(nil), r.statuses...)
}

func (r *Runtime) reloadServerLocked(ctx context.Context, cfg mcp.ServerConfig) ServerStatus {
	old := r.active[cfg.Name]
	status := ServerStatus{Name: cfg.Name, Agent: r.agent, Source: r.sources[cfg.Name], State: StateReloadFailed, Retained: old != nil}
	if r.policy == nil || r.policy.DecideMCPServerIdentity(r.serverIdentityLocked(cfg)).Permission != security.MCPAllow || validateRuntimeConfig(cfg) != nil {
		return r.setStatusLocked(status)
	}
	if old == nil && r.policy.MaxMCPConnections > 0 && r.connected >= r.policy.MaxMCPConnections {
		status.State = StateLimitReached
		return r.setStatusLocked(status)
	}
	for attempt := 1; attempt <= defaultReloadAttempts; attempt++ {
		status.Attempts = attempt
		client, definitions, names, err := r.prepareServerLocked(ctx, cfg)
		if err == nil {
			var remove []string
			if old != nil {
				remove = old.toolNames
			}
			if r.registry != nil {
				err = r.registry.Replace(remove, definitions)
			} else {
				err = errors.New("tool registry is unavailable")
			}
			if err == nil {
				r.active[cfg.Name] = &activeServer{config: cloneServerConfig(cfg), client: client, toolNames: names}
				if old == nil {
					r.connected++
				} else {
					_ = old.client.Close()
				}
				status.State, status.Tools, status.Retained = StateConnected, len(names), false
				return r.setStatusLocked(status)
			}
			_ = client.Close()
		}
		if attempt < defaultReloadAttempts {
			timer := time.NewTimer(defaultReloadBackoff << (attempt - 1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return r.setStatusLocked(status)
			case <-timer.C:
			}
		}
	}
	return r.setStatusLocked(status)
}

func (r *Runtime) serverIdentityLocked(cfg mcp.ServerConfig) security.MCPServerIdentity {
	origin := "discovered"
	if _, ok := r.configured[cfg.Name]; ok {
		origin = "configured"
	}
	return serverIdentity(cfg, origin)
}

func serverIdentity(cfg mcp.ServerConfig, origin string) security.MCPServerIdentity {
	return security.MCPServerIdentity{
		Name: cfg.Name, Origin: origin, Transport: string(cfg.Transport), Command: cfg.Command,
		Args: append([]string(nil), cfg.Args...), URL: cfg.URL,
	}
}

func (r *Runtime) prepareServerLocked(ctx context.Context, cfg mcp.ServerConfig) (RuntimeClient, []*tool.Definition, []string, error) {
	cfg.Permission = string(tool.PermRequireApproval)
	client, err := r.factory(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	err = client.Connect(callCtx)
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, nil, nil, err
	}
	callCtx, cancel = context.WithTimeout(ctx, r.timeout)
	tools, err := client.ListTools(callCtx)
	cancel()
	definitions, names, ok := toolDefinitions(cfg.Name, client, tools)
	if err != nil || !ok {
		_ = client.Close()
		if err == nil {
			err = errors.New("invalid MCP tool definitions")
		}
		return nil, nil, nil, err
	}
	if r.transform != nil {
		definitions = r.transform(definitions)
	}
	return client, definitions, names, nil
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
	status.Agent = r.agent
	if status.Source == "" {
		status.Source = r.sources[status.Name]
	}
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

func (r *Runtime) removeStatusLocked(name string) {
	for i, status := range r.statuses {
		if status.Name == name {
			r.statuses = append(r.statuses[:i], r.statuses[i+1:]...)
			return
		}
	}
}

func configMap(configs []mcp.ServerConfig) map[string]mcp.ServerConfig {
	out := make(map[string]mcp.ServerConfig, len(configs))
	for _, cfg := range configs {
		out[cfg.Name] = cloneServerConfig(cfg)
	}
	return out
}

func mapConfigs(configs map[string]mcp.ServerConfig) []mcp.ServerConfig {
	out := make([]mcp.ServerConfig, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, cloneServerConfig(cfg))
	}
	return out
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

func toolDefinitions(server string, client RuntimeClient, tools []mcp.ToolInfo) ([]*tool.Definition, []string, bool) {
	type pendingTool struct {
		name string
		info mcp.ToolInfo
	}
	pending := make([]pendingTool, 0, len(tools))
	for _, info := range tools {
		name := ToolName(server, info.Name)
		for _, item := range pending {
			if item.name == name {
				return nil, nil, false
			}
		}
		if info.Name == "" {
			return nil, nil, false
		}
		pending = append(pending, pendingTool{name: name, info: info})
	}
	definitions := make([]*tool.Definition, 0, len(pending))
	names := make([]string, 0, len(pending))
	for _, item := range pending {
		remoteName := item.info.Name
		definitions = append(definitions, &tool.Definition{
			Name: item.name, Description: item.info.Description,
			Parameters: item.info.InputSchema, Permission: tool.PermRequireApproval,
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				return client.CallTool(ctx, remoteName, args)
			},
		})
		names = append(names, item.name)
	}
	return definitions, names, true
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
