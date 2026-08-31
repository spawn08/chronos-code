// Package mcpdiscover implements PRD P4-002: MCP server auto-discovery.
// It scans a project directory for MCP server configurations from known
// tool config files (Cursor, VS Code, Claude Code, package.json) and
// converts them into chronos mcp.ServerConfig entries.
package mcpdiscover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/spawn08/chronos/engine/mcp"
)

// configPaths lists the files to scan, in priority order. When the same
// server name appears in multiple files, the first discovered wins.
var configPaths = []string{
	".cursor/mcp.json",
	".vscode/mcp.json",
	"package.json",
	".claude/mcp.json",
}

// Discover scans root for MCP server configs from known tool config files.
// It returns all discovered servers deduplicated by name (first wins).
// Errors in individual files are silently skipped.
func Discover(root string) []mcp.ServerConfig {
	seen := make(map[string]bool)
	var out []mcp.ServerConfig
	for _, rel := range configPaths {
		path := filepath.Join(root, rel)
		servers, err := DiscoverFromFile(path)
		if err != nil {
			continue
		}
		for _, s := range servers {
			if !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// configFile represents the JSON structure shared by all supported config
// formats: a top-level object with an "mcpServers" key.
type configFile struct {
	MCPServers map[string]serverEntry `json:"mcpServers"`
}

type serverEntry struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Type      string   `json:"type"`
	Transport string   `json:"transport"`
	URL       string   `json:"url"`
}

// DiscoverFromFile parses a single config file and returns the MCP servers
// found. Returns an empty slice (not an error) for missing files.
func DiscoverFromFile(path string) ([]mcp.ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, err
	}
	if len(cf.MCPServers) == 0 {
		return nil, nil
	}

	var out []mcp.ServerConfig
	for name, entry := range cf.MCPServers {
		sc := toServerConfig(name, entry)
		if sc.Command == "" && sc.URL == "" {
			continue
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func toServerConfig(name string, e serverEntry) mcp.ServerConfig {
	transport := mcp.Transport(e.Transport)
	if transport == "" {
		transport = mcp.Transport(e.Type)
	}
	if transport == "" {
		if e.URL != "" {
			transport = mcp.TransportSSE
		} else if e.Command != "" {
			transport = mcp.TransportStdio
		}
	}
	return mcp.ServerConfig{
		Name:       name,
		Transport:  transport,
		Command:    e.Command,
		Args:       e.Args,
		URL:        e.URL,
		Permission: "require_approval",
	}
}
