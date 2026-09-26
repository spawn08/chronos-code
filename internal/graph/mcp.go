package graph

import (
	"github.com/spawn08/chronos/engine/interop/mcpserver"
	"github.com/spawn08/chronos/engine/tool"
)

// MCPServerName is the server name advertised to MCP hosts.
const MCPServerName = "chronos-code-indexer"

// NewMCPServer exposes the scope's graph and impact tools to MCP hosts
// (M9): the same definitions agents call in process, so an external agent
// gets the same results. Every tool is read-only; each is exposed with its
// own permission (allow), never elevated.
func NewMCPServer(s *IndexScope, version string) *mcpserver.Server {
	srv := mcpserver.New(MCPServerName, mcpserver.WithVersion(version), mcpserver.WithDefaultPermission(tool.PermRequireApproval))
	for _, def := range append(s.Tools(), s.ImpactTools()...) {
		srv.Expose(def)
	}
	return srv
}
