// Package security enforces filesystem path allowlists/denylists and shell
// command restrictions at tool-call time. It implements a chronos hooks.Hook
// (Guard) that runs on hooks.EventToolCallBefore, before the chronos sdk/agent
// tool-calling loop executes a tool, so a disallowed call is blocked before it
// ever runs (see sdk/agent/agent.go executeToolCalls: a.Hooks.Before is called
// and, on error, the tool is never executed).
package security

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"
	"gopkg.in/yaml.v3"
)

// defaultMaxExecSeconds is applied when the shell.max_execution_time_sec
// section is missing or zero.
const defaultMaxExecSeconds = 300

// Policy is the flattened, in-memory representation of the security policy
// YAML (see internal/defaults/security.yaml for the on-disk shape).
type Policy struct {
	WritablePaths   []string
	ReadablePaths   []string
	DeniedPaths     []string
	AllowedCommands []string
	DeniedPatterns  []string
	MaxExecSeconds  int
	SecretPatterns  []string // carried through for reuse by another package
	autoAllow       []*regexp.Regexp
	confirm         []*regexp.Regexp
	neverAllow      []*regexp.Regexp
}

// policyYAML mirrors the on-disk YAML shape.
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
		MaxExecutionSecs int      `yaml:"max_execution_time_sec"`
	} `yaml:"shell"`
	Secrets struct {
		ScanOutput bool     `yaml:"scan_output"`
		Patterns   []string `yaml:"patterns"`
	} `yaml:"secrets"`
}

// LoadPolicy parses the security policy YAML and flattens it into a *Policy,
// applying sane defaults for any missing section.
func LoadPolicy(data []byte) (*Policy, error) {
	var raw policyYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("security: parse policy yaml: %w", err)
	}

	p := &Policy{
		WritablePaths:   raw.Filesystem.WritablePaths,
		ReadablePaths:   raw.Filesystem.ReadablePaths,
		DeniedPaths:     raw.Filesystem.DeniedPaths,
		AllowedCommands: raw.Shell.AllowedCommands,
		DeniedPatterns:  raw.Shell.DeniedPatterns,
		MaxExecSeconds:  raw.Shell.MaxExecutionSecs,
		SecretPatterns:  raw.Secrets.Patterns,
	}
	if p.MaxExecSeconds <= 0 {
		p.MaxExecSeconds = defaultMaxExecSeconds
	}

	var err error
	if p.autoAllow, err = compilePatterns("shell.auto_allow", raw.Shell.AutoAllow); err != nil {
		return nil, err
	}
	if p.confirm, err = compilePatterns("shell.confirm", raw.Shell.Confirm); err != nil {
		return nil, err
	}
	if p.neverAllow, err = compilePatterns("shell.never_allow", raw.Shell.NeverAllow); err != nil {
		return nil, err
	}
	return p, nil
}

func compilePatterns(field string, patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for i, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("security: invalid %s[%d] regex %q: %w", field, i, pattern, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// Guard is a hooks.Hook that enforces Policy at tool-call time.
type Guard struct {
	policy *Policy
	root   string
	store  storage.Storage
}

// NewGuard builds a Guard for policy, resolving relative tool-arg paths
// against root (the workspace root). store may be nil, in which case blocked
// calls are not audit-logged.
func NewGuard(policy *Policy, root string, store storage.Storage) *Guard {
	return &Guard{policy: policy, root: root, store: store}
}

// Before implements hooks.Hook. It only acts on hooks.EventToolCallBefore and
// returns a non-nil error to block disallowed filesystem or shell tool calls.
func (g *Guard) Before(ctx context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventToolCallBefore {
		return nil
	}

	args, _ := evt.Input.(map[string]any)

	var blockErr error
	switch evt.Name {
	case "file_read", "file_write", "file_list", "file_glob", "file_grep":
		blockErr = g.checkFileArgs(evt.Name, args)
	case "shell":
		blockErr = g.checkShellArgs(args, false)
	case "shell_auto":
		blockErr = g.checkShellArgs(args, true)
	default:
		return nil
	}

	if blockErr != nil {
		g.audit(ctx, evt.Name, blockErr.Error(), args)
		return blockErr
	}
	return nil
}

// After implements hooks.Hook. The security guard has nothing to do after a
// tool executes.
func (g *Guard) After(ctx context.Context, evt *hooks.Event) error { return nil }

// checkFileArgs enforces the denied-path and (for file_write) writable-path
// rules against a "path" string argument, when present, and — for tools like
// file_glob whose schema has no "path" at all, only a "pattern" — against
// the glob pattern itself. Checking a glob pattern against DeniedPaths is
// necessarily a heuristic (matchesAnyGlob is a pragmatic matcher, not a full
// glob-overlap solver: an overly broad pattern like "**/*" that would
// incidentally sweep up a denied file isn't caught), but it closes the
// direct case of an agent explicitly globbing for a denied file, e.g.
// file_glob with pattern="**/.env".
func (g *Guard) checkFileArgs(toolName string, args map[string]any) error {
	path, hasPath := args["path"].(string)
	pattern, hasPattern := args["pattern"].(string)

	if !hasPath || path == "" {
		if hasPattern && pattern != "" && matchesAnyGlob(g.policy.DeniedPaths, pattern) {
			return fmt.Errorf("security: pattern %q is denied by policy (matches a denied path pattern)", pattern)
		}
		return nil
	}

	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(g.root, resolved)
	}

	if matchesAnyGlob(g.policy.DeniedPaths, resolved) {
		return fmt.Errorf("security: access to %q is denied by policy (matches a denied path pattern)", path)
	}

	if toolName == "file_write" && len(g.policy.WritablePaths) > 0 {
		if !isUnderAnyRoot(g.root, g.policy.WritablePaths, resolved) {
			return fmt.Errorf("security: write to %q is denied (outside all writable_paths)", path)
		}
	}

	return nil
}

// isUnderAnyRoot reports whether resolved falls under at least one of the
// writable-path roots (each resolved relative to workspace root if relative).
func isUnderAnyRoot(root string, writablePaths []string, resolved string) bool {
	for _, wp := range writablePaths {
		wpResolved := wp
		if !filepath.IsAbs(wpResolved) {
			wpResolved = filepath.Join(root, wpResolved)
		}
		wpResolved = filepath.Clean(wpResolved)
		resolvedClean := filepath.Clean(resolved)
		if resolvedClean == wpResolved {
			return true
		}
		rel, err := filepath.Rel(wpResolved, resolvedClean)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "") {
			return true
		}
	}
	return false
}

// checkShellArgs enforces the denied-pattern, never_allow, and
// allowed-command rules against a "command" string argument, when present.
// Unlike neverAllow's other consumer (PermissionChecker.Check, which only
// decides whether the interactive TUI should prompt a human), this runs
// inside the Guard hook wired into every tool call regardless of approval
// mode — so a never_allow rule is the only tier that reliably blocks a
// command even when approvals are auto-accepted (yolo mode or unattended
// runs), matching its documented "resolve to deny" intent.
func (g *Guard) checkShellArgs(args map[string]any, requireAllowlisted bool) error {
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return nil
	}

	lowerCmd := strings.ToLower(command)
	for _, pattern := range g.policy.DeniedPatterns {
		if strings.Contains(lowerCmd, strings.ToLower(pattern)) {
			return fmt.Errorf("security: shell command denied (matches denied pattern %q)", pattern)
		}
	}

	if pattern := firstMatchingRegex(g.policy.neverAllow, command); pattern != nil {
		return fmt.Errorf("security: shell command denied (matches never_allow pattern %q); if this was reading or searching a file, use file_read (with start_line/end_line) or file_grep (with regex/recursive search) instead of shell", pattern.String())
	}

	if requireAllowlisted && !g.shellCommandAllowed(command) {
		return fmt.Errorf("security: shell command %q is not in the allowed_commands list", firstShellCommand(command))
	}

	return nil
}

// firstMatchingRegex returns the first pattern in patterns matching value,
// or nil if none match.
func firstMatchingRegex(patterns []*regexp.Regexp, value string) *regexp.Regexp {
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return pattern
		}
	}
	return nil
}

func (g *Guard) shellCommandAllowed(command string) bool {
	if len(g.policy.AllowedCommands) == 0 {
		return true
	}
	first := firstShellCommand(command)
	for _, allowed := range g.policy.AllowedCommands {
		if first == allowed {
			return true
		}
	}
	return false
}

func firstShellCommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// audit best-effort records a blocked tool call to storage. Any error from
// the audit write itself is ignored and never changes the block/allow
// decision.
func (g *Guard) audit(ctx context.Context, resource, reason string, args map[string]any) {
	if g.store == nil {
		return
	}
	log := &storage.AuditLog{
		ID:        newAuditID(),
		SessionID: storage.SessionFromContext(ctx),
		Actor:     "security-guard",
		Action:    "block",
		Resource:  resource,
		Detail: map[string]any{
			"reason": reason,
			"args":   args,
		},
		CreatedAt: time.Now(),
	}
	_ = g.store.AppendAuditLog(ctx, log)
}

// newAuditID generates a random hex identifier for an audit log entry.
func newAuditID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// matchesAnyGlob is a pragmatic (not fully general) glob matcher good enough
// for the default denied_paths pattern set (basename globs like ".env",
// ".env.*", "*.pem", and "**/"-prefixed suffix globs like "**/*.pem",
// "**/secrets/**").
func matchesAnyGlob(patterns []string, relOrAbsPath string) bool {
	base := filepath.Base(relOrAbsPath)
	for _, pattern := range patterns {
		if matchesOneGlob(pattern, base, relOrAbsPath) {
			return true
		}
	}
	return false
}

func matchesOneGlob(pattern, base, fullPath string) bool {
	if idx := strings.LastIndex(pattern, "**/"); idx >= 0 {
		suffixPattern := pattern[idx+len("**/"):]
		if suffixPattern == "" {
			// e.g. "**/secrets/**" -> nothing after the last "**/"; treat the
			// stripped prefix as a path-containment check below.
		} else if ok, _ := filepath.Match(suffixPattern, base); ok {
			return true
		}
		stripped := strings.ReplaceAll(pattern, "**/", "")
		if stripped != "" {
			if strings.HasSuffix(fullPath, stripped) {
				return true
			}
			if ok, _ := filepath.Match(stripped, fullPath); ok {
				return true
			}
			// "**/secrets/**" style: match if the path contains the
			// stripped-of-trailing-** directory segment anywhere.
			trimmed := strings.TrimSuffix(stripped, "/**")
			trimmed = strings.TrimSuffix(trimmed, "**")
			if trimmed != "" {
				sep := string(filepath.Separator)
				if strings.Contains(fullPath, sep+trimmed+sep) ||
					strings.HasPrefix(fullPath, trimmed+sep) ||
					strings.Contains(fullPath, "/"+trimmed+"/") {
					return true
				}
			}
		}
		return false
	}

	if strings.Contains(pattern, "/") {
		if ok, _ := filepath.Match(pattern, fullPath); ok {
			return true
		}
		return false
	}

	if ok, _ := filepath.Match(pattern, base); ok {
		return true
	}
	return false
}
