package security

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultMaxExecSeconds = 300

// MCPPermission is the server-level action required before an MCP server may
// be launched. Tool permissions remain independently enforced after launch.
type MCPPermission string

const (
	MCPAllow           MCPPermission = "allow"
	MCPRequireApproval MCPPermission = "require_approval"
	MCPDeny            MCPPermission = "deny"
)

// MCPDecision contains only a permission and a fixed, non-sensitive reason.
type MCPDecision struct {
	Permission MCPPermission
	Reason     string
}

// Policy is the effective in-memory security policy.
type Policy struct {
	WritablePaths          []string
	ReadablePaths          []string
	DeniedPaths            []string
	AllowedCommands        []string
	DeniedPatterns         []string
	MaxExecSeconds         int
	SecretPatterns         []string
	ScanOutput             bool
	DeniedMCPServers       []string
	TrustedMCPServers      []string
	MCPDefaultPermission   MCPPermission
	MaxMCPConnections      int
	autoAllowPatterns      []string
	confirmPatterns        []string
	neverAllowPatterns     []string
	autoAllow              []*regexp.Regexp
	confirm                []*regexp.Regexp
	neverAllow             []*regexp.Regexp
	writablePathsSpecified bool
	readablePathsSpecified bool
	allowedCommandsSet     bool
}

type policyYAML struct {
	Version    string `yaml:"version"`
	Filesystem struct {
		WritablePaths []string `yaml:"writable_paths"`
		ReadablePaths []string `yaml:"readable_paths"`
		DeniedPaths   []string `yaml:"denied_paths"`
	} `yaml:"filesystem"`
	Shell struct {
		AllowedCommands  []string `yaml:"allowed_commands"`
		DeniedPatterns   []string `yaml:"denied_patterns"`
		AutoAllow        []string `yaml:"auto_allow"`
		Confirm          []string `yaml:"confirm"`
		NeverAllow       []string `yaml:"never_allow"`
		MaxExecutionSecs *int     `yaml:"max_execution_time_sec"`
	} `yaml:"shell"`
	Secrets struct {
		ScanOutput *bool    `yaml:"scan_output"`
		Patterns   []string `yaml:"patterns"`
	} `yaml:"secrets"`
	MCP struct {
		DeniedServers     []string       `yaml:"denied_servers"`
		TrustedServers    []string       `yaml:"trusted_servers"`
		DefaultPermission *MCPPermission `yaml:"default_permission"`
		MaxConnections    *int           `yaml:"max_connections"`
	} `yaml:"mcp"`
}

// Overlay identifies one optional policy layer for actionable errors.
type Overlay struct {
	Source string
	Data   []byte
}

// LoadPolicy parses one standalone policy. Runtime startup should use
// ResolvePolicy so this parser cannot accidentally replace the embedded floor.
func LoadPolicy(data []byte) (*Policy, error) {
	raw, err := parsePolicy(data)
	if err != nil {
		return nil, err
	}
	p := policyFromRaw(raw)
	if err := p.compile(); err != nil {
		return nil, err
	}
	return p, nil
}

// ResolvePolicy applies overlays in order. Each layer may add denials and
// confirmations or narrow allow scopes, but cannot relax the current policy.
func ResolvePolicy(floorData []byte, overlays ...Overlay) (*Policy, error) {
	floorRaw, err := parsePolicy(floorData)
	if err != nil {
		return nil, fmt.Errorf("embedded security floor: %w", err)
	}
	effective := policyFromRaw(floorRaw)
	if err := effective.compile(); err != nil {
		return nil, fmt.Errorf("embedded security floor: %w", err)
	}
	if effective.MCPDefaultPermission == "" {
		effective.MCPDefaultPermission = MCPRequireApproval
	}
	for _, overlay := range overlays {
		raw, err := parsePolicy(overlay.Data)
		if err != nil {
			return nil, fmt.Errorf("%s security overlay: %w", safeSource(overlay.Source), err)
		}
		if err := validateRawPatterns(raw); err != nil {
			return nil, fmt.Errorf("%s security overlay: %w", safeSource(overlay.Source), err)
		}
		if err := applyOverlay(effective, raw); err != nil {
			return nil, fmt.Errorf("%s security overlay: %w", safeSource(overlay.Source), err)
		}
	}
	if err := effective.compile(); err != nil {
		return nil, err
	}
	return effective, nil
}

func validateRawPatterns(raw *policyYAML) error {
	for _, field := range []struct {
		name     string
		patterns []string
	}{
		{"shell.auto_allow", raw.Shell.AutoAllow},
		{"shell.confirm", raw.Shell.Confirm},
		{"shell.never_allow", raw.Shell.NeverAllow},
		{"secrets.patterns", raw.Secrets.Patterns},
	} {
		if _, err := compilePatterns(field.name, field.patterns); err != nil {
			return err
		}
	}
	return nil
}

func parsePolicy(data []byte) (*policyYAML, error) {
	var raw policyYAML
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse policy yaml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse policy yaml: %w", err)
		}
		return nil, fmt.Errorf("parse policy yaml: multiple documents are not supported")
	}
	return &raw, nil
}

func policyFromRaw(raw *policyYAML) *Policy {
	p := &Policy{
		WritablePaths:          cloneStrings(raw.Filesystem.WritablePaths),
		ReadablePaths:          cloneStrings(raw.Filesystem.ReadablePaths),
		DeniedPaths:            cloneStrings(raw.Filesystem.DeniedPaths),
		AllowedCommands:        cloneStrings(raw.Shell.AllowedCommands),
		DeniedPatterns:         cloneStrings(raw.Shell.DeniedPatterns),
		SecretPatterns:         cloneStrings(raw.Secrets.Patterns),
		DeniedMCPServers:       cloneStrings(raw.MCP.DeniedServers),
		TrustedMCPServers:      cloneStrings(raw.MCP.TrustedServers),
		autoAllowPatterns:      cloneStrings(raw.Shell.AutoAllow),
		confirmPatterns:        cloneStrings(raw.Shell.Confirm),
		neverAllowPatterns:     cloneStrings(raw.Shell.NeverAllow),
		writablePathsSpecified: raw.Filesystem.WritablePaths != nil,
		readablePathsSpecified: raw.Filesystem.ReadablePaths != nil,
		allowedCommandsSet:     raw.Shell.AllowedCommands != nil,
	}
	if raw.Shell.MaxExecutionSecs == nil || *raw.Shell.MaxExecutionSecs <= 0 {
		p.MaxExecSeconds = defaultMaxExecSeconds
	} else {
		p.MaxExecSeconds = *raw.Shell.MaxExecutionSecs
	}
	if raw.Secrets.ScanOutput != nil {
		p.ScanOutput = *raw.Secrets.ScanOutput
	}
	if raw.MCP.DefaultPermission != nil {
		p.MCPDefaultPermission = *raw.MCP.DefaultPermission
	}
	if raw.MCP.MaxConnections != nil {
		p.MaxMCPConnections = *raw.MCP.MaxConnections
	}
	return p
}

func applyOverlay(effective *Policy, raw *policyYAML) error {
	if raw.Filesystem.WritablePaths != nil {
		if err := validateNarrowerPaths("filesystem.writable_paths", effective.WritablePaths, raw.Filesystem.WritablePaths); err != nil {
			return err
		}
		effective.WritablePaths = cloneStrings(raw.Filesystem.WritablePaths)
		effective.writablePathsSpecified = true
	}
	if raw.Filesystem.ReadablePaths != nil {
		if err := validateNarrowerPaths("filesystem.readable_paths", effective.ReadablePaths, raw.Filesystem.ReadablePaths); err != nil {
			return err
		}
		effective.ReadablePaths = cloneStrings(raw.Filesystem.ReadablePaths)
		effective.readablePathsSpecified = true
	}
	effective.DeniedPaths = union(effective.DeniedPaths, raw.Filesystem.DeniedPaths)

	if raw.Shell.AllowedCommands != nil {
		if extra := firstNotIn(raw.Shell.AllowedCommands, effective.AllowedCommands); extra >= 0 {
			return fmt.Errorf("shell.allowed_commands[%d] broadens the embedded/current allowlist", extra)
		}
		effective.AllowedCommands = cloneStrings(raw.Shell.AllowedCommands)
		effective.allowedCommandsSet = true
	}
	if raw.Shell.AutoAllow != nil {
		if extra := firstNotIn(raw.Shell.AutoAllow, effective.autoAllowPatterns); extra >= 0 {
			return fmt.Errorf("shell.auto_allow[%d] weakens required approval", extra)
		}
		effective.autoAllowPatterns = cloneStrings(raw.Shell.AutoAllow)
	}
	effective.DeniedPatterns = union(effective.DeniedPatterns, raw.Shell.DeniedPatterns)
	effective.confirmPatterns = union(effective.confirmPatterns, raw.Shell.Confirm)
	effective.neverAllowPatterns = union(effective.neverAllowPatterns, raw.Shell.NeverAllow)
	if raw.Shell.MaxExecutionSecs != nil {
		if *raw.Shell.MaxExecutionSecs <= 0 {
			return fmt.Errorf("shell.max_execution_time_sec must be positive")
		}
		if effective.MaxExecSeconds > 0 && *raw.Shell.MaxExecutionSecs > effective.MaxExecSeconds {
			return fmt.Errorf("shell.max_execution_time_sec cannot exceed embedded/current limit")
		}
		effective.MaxExecSeconds = *raw.Shell.MaxExecutionSecs
	}

	if raw.Secrets.ScanOutput != nil && effective.ScanOutput && !*raw.Secrets.ScanOutput {
		return fmt.Errorf("secrets.scan_output cannot disable embedded/current scanning")
	}
	if raw.Secrets.ScanOutput != nil {
		effective.ScanOutput = *raw.Secrets.ScanOutput
	}
	effective.SecretPatterns = union(effective.SecretPatterns, raw.Secrets.Patterns)

	effective.DeniedMCPServers = union(effective.DeniedMCPServers, raw.MCP.DeniedServers)
	if raw.MCP.TrustedServers != nil {
		if extra := firstNotIn(raw.MCP.TrustedServers, effective.TrustedMCPServers); extra >= 0 {
			return fmt.Errorf("mcp.trusted_servers[%d] adds trust not present in the embedded/current policy", extra)
		}
		effective.TrustedMCPServers = cloneStrings(raw.MCP.TrustedServers)
	}
	effective.TrustedMCPServers = difference(effective.TrustedMCPServers, effective.DeniedMCPServers)
	if raw.MCP.DefaultPermission != nil {
		if !validMCPPermission(*raw.MCP.DefaultPermission) {
			return fmt.Errorf("mcp.default_permission must be allow, require_approval, or deny")
		}
		if permissionStrength(*raw.MCP.DefaultPermission) < permissionStrength(effective.MCPDefaultPermission) {
			return fmt.Errorf("mcp.default_permission cannot weaken embedded/current approval")
		}
		effective.MCPDefaultPermission = *raw.MCP.DefaultPermission
	}
	if raw.MCP.MaxConnections != nil {
		if *raw.MCP.MaxConnections <= 0 {
			return fmt.Errorf("mcp.max_connections must be positive")
		}
		if effective.MaxMCPConnections > 0 && *raw.MCP.MaxConnections > effective.MaxMCPConnections {
			return fmt.Errorf("mcp.max_connections cannot exceed embedded/current limit")
		}
		effective.MaxMCPConnections = *raw.MCP.MaxConnections
	}
	return nil
}

// DecideMCPServer resolves a server name without including names, endpoints,
// arguments, or credentials in its audit-safe reason.
func (p *Policy) DecideMCPServer(name string) MCPDecision {
	if contains(p.DeniedMCPServers, name) {
		return MCPDecision{Permission: MCPDeny, Reason: "server denied by security policy"}
	}
	if contains(p.TrustedMCPServers, name) {
		return MCPDecision{Permission: MCPAllow, Reason: "server trusted by security policy"}
	}
	permission := p.MCPDefaultPermission
	if permission == "" {
		permission = MCPRequireApproval
	}
	return MCPDecision{Permission: permission, Reason: "unrecognized server uses default security policy"}
}

// AllowMCPServerSession grants in-memory trust for name for this process.
// Denied servers cannot be trusted, and the grant is not written to disk.
func (p *Policy) AllowMCPServerSession(name string) error {
	if p == nil {
		return fmt.Errorf("security policy is not configured")
	}
	if contains(p.DeniedMCPServers, name) {
		return fmt.Errorf("server denied by security policy")
	}
	if !contains(p.TrustedMCPServers, name) {
		p.TrustedMCPServers = append(p.TrustedMCPServers, name)
	}
	return nil
}

func (p *Policy) compile() error {
	var err error
	if p.autoAllow, err = compilePatterns("shell.auto_allow", p.autoAllowPatterns); err != nil {
		return err
	}
	if p.confirm, err = compilePatterns("shell.confirm", p.confirmPatterns); err != nil {
		return err
	}
	if p.neverAllow, err = compilePatterns("shell.never_allow", p.neverAllowPatterns); err != nil {
		return err
	}
	if _, err = compilePatterns("secrets.patterns", p.SecretPatterns); err != nil {
		return err
	}
	if p.MCPDefaultPermission != "" && !validMCPPermission(p.MCPDefaultPermission) {
		return fmt.Errorf("security: mcp.default_permission must be allow, require_approval, or deny")
	}
	if p.MaxMCPConnections < 0 {
		return fmt.Errorf("security: mcp.max_connections must be positive when specified")
	}
	return nil
}

func compilePatterns(field string, patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for i, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("security: invalid %s[%d] regex: %w", field, i, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

func (p *Policy) readablePathsConfigured() bool {
	return p.readablePathsSpecified || len(p.ReadablePaths) > 0
}

func (p *Policy) writablePathsConfigured() bool {
	return p.writablePathsSpecified || len(p.WritablePaths) > 0
}

func validateNarrowerPaths(field string, current, proposed []string) error {
	for i, path := range proposed {
		if path == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
		inside := false
		for _, allowed := range current {
			if pathWithin(allowed, path) {
				inside = true
				break
			}
		}
		if !inside {
			return fmt.Errorf("%s[%d] broadens the embedded/current scope", field, i)
		}
	}
	return nil
}

func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if filepath.IsAbs(parent) != filepath.IsAbs(child) {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func union(current, additions []string) []string {
	out := cloneStrings(current)
	for _, value := range additions {
		if !contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func difference(values, denied []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !contains(denied, value) {
			out = append(out, value)
		}
	}
	return out
}

func firstNotIn(values, allowed []string) int {
	for i, value := range values {
		if !contains(allowed, value) {
			return i
		}
	}
	return -1
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

func validMCPPermission(permission MCPPermission) bool {
	return permission == MCPAllow || permission == MCPRequireApproval || permission == MCPDeny
}

func permissionStrength(permission MCPPermission) int {
	switch permission {
	case MCPDeny:
		return 2
	case MCPRequireApproval, "":
		return 1
	default:
		return 0
	}
}

func safeSource(source string) string {
	if source == "user" || source == "project" {
		return source
	}
	return "configured"
}
