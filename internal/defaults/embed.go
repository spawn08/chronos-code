package defaults

import "embed"

//go:embed all:agents all:skills all:guardrails config.yaml security.yaml mcp-servers.yaml tools.yaml routing.yaml
var FS embed.FS

func ReadFile(name string) ([]byte, error) {
	return FS.ReadFile(name)
}
