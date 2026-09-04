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
	clients   []RuntimeClient
	statuses  []ServerStatus
	closeOnce sync.Once
	closeErr  error
}

func (r *Runtime) Statuses() []ServerStatus {
	return append([]ServerStatus(nil), r.statuses...)
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		var errs []error
		for _, client := range r.clients {
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
	runtime := &Runtime{}
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	if factory == nil {
		factory = NewClient
	}

	connected := 0
	for _, cfg := range mergeServerConfigs(configured, discovered) {
		if policy == nil {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateApprovalRequired})
			continue
		}
		decision := policy.DecideMCPServer(cfg.Name)
		if decision.Permission == security.MCPDeny {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateDenied})
			continue
		}
		if decision.Permission != security.MCPAllow {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateApprovalRequired})
			continue
		}
		if policy.MaxMCPConnections > 0 && connected >= policy.MaxMCPConnections {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateLimitReached})
			continue
		}
		if validateRuntimeConfig(cfg) != nil {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateInvalid})
			continue
		}

		// Server trust permits startup, not unreviewed tool execution.
		cfg.Permission = string(tool.PermRequireApproval)
		client, err := factory(cfg)
		if err != nil {
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateConnectFailed})
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		err = client.Connect(callCtx)
		cancel()
		if err != nil {
			_ = client.Close()
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateConnectFailed})
			continue
		}

		callCtx, cancel = context.WithTimeout(ctx, timeout)
		tools, err := client.ListTools(callCtx)
		cancel()
		if err != nil || !registerTools(registry, cfg.Name, client, tools) {
			_ = client.Close()
			runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateToolsFailed})
			continue
		}
		runtime.clients = append(runtime.clients, client)
		connected++
		runtime.statuses = append(runtime.statuses, ServerStatus{Name: cfg.Name, State: StateConnected, Tools: len(tools)})
	}
	return runtime
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
