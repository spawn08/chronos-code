// Package mcpdiscover implements PRD P4-002: MCP server auto-discovery.
// It scans a project directory for MCP server configurations from known
// tool config files (Cursor, VS Code, Claude Code, package.json) and
// converts them into chronos mcp.ServerConfig entries.
package mcpdiscover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spawn08/chronos/engine/mcp"
)

// projectConfigPaths lists project files in priority order. When the same
// server name appears in multiple files, the first discovered wins.
var projectConfigPaths = []string{
	".mcp.json",
	".cursor/mcp.json",
	".vscode/mcp.json",
	"package.json",
	".claude/mcp.json",
}

const MaxServersPerSource = 128

type SourceState string

const (
	SourceHealthy SourceState = "healthy"
	SourceMissing SourceState = "missing"
	SourceInvalid SourceState = "invalid"
)

// SourceStatus reports one independently parsed discovery source. Servers is
// retained by Watch when a later version of that source is malformed.
type SourceStatus struct {
	Path          string
	State         SourceState
	Servers       []mcp.ServerConfig
	Diagnostic    string
	LastKnownGood bool
}

// Snapshot is an immutable discovery result. Healthy sources remain available
// when a sibling source is malformed; Err joins source diagnostics for callers
// that only need aggregate compatibility.
type Snapshot struct {
	Servers []mcp.ServerConfig
	Sources []SourceStatus
	Err     error
}

// Discover scans root for MCP server configs from known tool config files.
// It returns all discovered servers deduplicated by name (first wins).
// Errors in individual files are silently skipped.
func Discover(root string) []mcp.ServerConfig {
	paths, err := configFilePaths(root)
	if err != nil {
		paths = projectFilePaths(root)
	}
	return discover(paths).Servers
}

// Load discovers all project and user MCP configs. Unlike Discover, Load
// reports malformed or unreadable sources so a runtime can retain its last
// known-good configuration.
func Load(root string) Snapshot {
	paths, err := configFilePaths(root)
	if err != nil {
		return Snapshot{Err: fmt.Errorf("resolve user MCP config: %w", err)}
	}
	return discover(paths)
}

func discover(paths []string) Snapshot {
	snapshot := Snapshot{Sources: make([]SourceStatus, 0, len(paths))}
	for _, path := range paths {
		servers, err := DiscoverFromFile(path)
		if err != nil {
			snapshot.Sources = append(snapshot.Sources, SourceStatus{Path: path, State: SourceInvalid, Diagnostic: err.Error()})
			snapshot.Err = errors.Join(snapshot.Err, err)
			continue
		}
		state := SourceHealthy
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			state = SourceMissing
		}
		snapshot.Sources = append(snapshot.Sources, SourceStatus{Path: path, State: state, Servers: servers})
	}
	snapshot.Servers = mergeSources(snapshot.Sources)
	return snapshot
}

func mergeSources(sources []SourceStatus) []mcp.ServerConfig {
	seen := make(map[string]bool)
	var out []mcp.ServerConfig
	for _, source := range sources {
		for _, server := range source.Servers {
			if !seen[server.Name] {
				seen[server.Name] = true
				out = append(out, cloneServerConfig(server))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SourceForServer returns the highest-priority source contributing name.
func (s Snapshot) SourceForServer(name string) string {
	for _, source := range s.Sources {
		for _, server := range source.Servers {
			if server.Name == name {
				return source.Path
			}
		}
	}
	return ""
}

func configFilePaths(root string) ([]string, error) {
	paths := projectFilePaths(root)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return append(paths, filepath.Join(home, ".chronos-code", "mcp.json")), nil
}

func projectFilePaths(root string) []string {
	paths := make([]string, 0, len(projectConfigPaths))
	for _, rel := range projectConfigPaths {
		paths = append(paths, filepath.Join(root, rel))
	}
	return paths
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
	data, err := readBoundedFile(path, MaxConfigBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read MCP config %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse MCP config %s: %w", path, err)
	}
	if len(cf.MCPServers) == 0 {
		return nil, nil
	}
	if len(cf.MCPServers) > MaxServersPerSource {
		return nil, fmt.Errorf("parse MCP config %s: %d servers exceeds limit %d", path, len(cf.MCPServers), MaxServersPerSource)
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
