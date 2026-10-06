package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// mcpStateNotConfigured marks a --require-mcp name that no MCP source defines.
const mcpStateNotConfigured = "not_configured"

// mcpStateMessages are the fixed error texts per state. Raw errors can hold
// URLs or header values, so they never reach the stream.
var mcpStateMessages = map[string]string{
	string(mcpdiscover.StateDenied):           "denied by security policy",
	string(mcpdiscover.StateApprovalRequired): "not approved for this run (use --mcp-config or --mcp-connect)",
	string(mcpdiscover.StateInvalid):          "invalid server configuration",
	string(mcpdiscover.StateLimitReached):     "MCP connection limit reached",
	string(mcpdiscover.StateConnectFailed):    "could not connect",
	string(mcpdiscover.StateToolsFailed):      "could not list or register tools",
	string(mcpdiscover.StateReloadFailed):     "reload failed",
	mcpStateNotConfigured:                     "no MCP source defines this server",
}

// mcpStatusReport returns the active agent's MCP servers. A required name
// that no source defines is listed as not_configured.
func mcpStatusReport(orch *orchestrator.Orchestrator, required []string) execution.MCPStatusPayload {
	active := orch.ActiveID()
	servers := []execution.MCPServerStatus{}
	known := map[string]bool{}
	for _, status := range orch.MCPStatuses() {
		if status.Agent != active {
			continue
		}
		known[status.Name] = true
		servers = append(servers, execution.MCPServerStatus{
			Name: status.Name, State: string(status.State), Tools: status.Tools,
			Error: mcpStateMessages[string(status.State)],
		})
	}
	for _, name := range required {
		if !known[name] {
			known[name] = true
			servers = append(servers, execution.MCPServerStatus{Name: name, State: mcpStateNotConfigured, Error: mcpStateMessages[mcpStateNotConfigured]})
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return execution.MCPStatusPayload{Servers: servers}
}

// requiredMCPFailure reports the required servers that are not connected,
// or nil when all are.
func requiredMCPFailure(report execution.MCPStatusPayload, required []string) error {
	if len(required) == 0 {
		return nil
	}
	want := make(map[string]bool, len(required))
	for _, name := range required {
		want[name] = true
	}
	var missing []string
	for _, server := range report.Servers {
		if want[server.Name] && server.State != string(mcpdiscover.StateConnected) {
			missing = append(missing, fmt.Sprintf("%s (%s)", server.Name, server.State))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("required MCP server(s) not connected: %s", strings.Join(missing, ", "))
}

// mcpUnavailableError is the terminal error for a failed --require-mcp check.
func mcpUnavailableError(err error) execution.Error {
	return execution.Error{Code: execution.ErrorMCPUnavailable, Category: execution.ErrorCategoryDependency, Retryable: true, Message: err.Error()}
}
