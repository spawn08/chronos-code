package security

import (
	"context"
	"regexp"
	"strings"

	"github.com/spawn08/chronos/engine/tool/builtins"
)

// Decision is the action required before a tool call may execute.
type Decision string

const (
	Auto    Decision = "auto"
	Confirm Decision = "confirm"
	Deny    Decision = "deny"
)

// PermissionChecker resolves argument-aware tool permissions without
// executing the tool.
type PermissionChecker struct {
	policy *Policy
	guard  *Guard
}

// NewPermissionChecker creates a checker rooted at the workspace directory.
func NewPermissionChecker(policy *Policy, root string) *PermissionChecker {
	if policy == nil {
		policy = &Policy{}
	}
	return &PermissionChecker{
		policy: policy,
		guard:  NewGuard(policy, root, nil),
	}
}

// Check returns the permission decision for a tool call. Yolo only promotes
// ordinary write and shell confirmations; hard denials and explicit confirm
// rules always take precedence.
func (c *PermissionChecker) Check(toolName string, args map[string]any, yolo bool) Decision {
	return c.CheckContext(context.Background(), toolName, args, yolo)
}

// CheckContext evaluates file policy against the request-scoped workspace.
func (c *PermissionChecker) CheckContext(ctx context.Context, toolName string, args map[string]any, yolo bool) Decision {
	switch toolName {
	case "file_read", "file_write", "file_list", "file_glob", "file_grep":
		if c.guard.checkFileArgsAtRoot(toolName, args, builtins.WorkspaceRoot(ctx, c.guard.root)) != nil {
			return Deny
		}
	case "shell", "shell_auto":
		if c.guard.checkShellArgs(args, toolName == "shell_auto") != nil {
			return Deny
		}
		command, _ := args["command"].(string)
		segments, compound := shellCommandSegments(command)
		for _, segment := range segments {
			if matchesAnyRegex(c.policy.neverAllow, segment) {
				return Deny
			}
		}
		if compound || invokesShellInterpreter(command) {
			return Confirm
		}
		if !c.guard.shellCommandAllowed(command) {
			return Confirm
		}
		if matchesAnyRegex(c.policy.confirm, command) {
			return Confirm
		}
		if matchesAnyRegex(c.policy.autoAllow, command) {
			return Auto
		}
	}

	switch toolName {
	case "codebase_map", "codebase_search", "graph_query", "find_callers",
		"find_implementations", "impact_analysis", "test_map", "co_change",
		"multi_resolution_view", "resolve_symbol",
		"file_read", "file_list", "file_glob", "file_grep", "semantic_search",
		"workspace_info":
		return Auto
	case "file_write", "shell":
		if yolo {
			return Auto
		}
		return Confirm
	case "shell_auto":
		return Auto
	default:
		return Confirm
	}
}

func shellCommandSegments(command string) ([]string, bool) {
	var segments []string
	start := 0
	quote := rune(0)
	escaped := false
	compound := false
	runes := []rune(command)
	for i, current := range runes {
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if current == quote {
				quote = 0
			}
			continue
		}
		if current == '\'' || current == '"' {
			quote = current
			continue
		}
		if current == '`' || current == '\n' || current == '\r' || current == ';' || current == '|' || current == '&' {
			compound = true
			if segment := strings.TrimSpace(string(runes[start:i])); segment != "" {
				segments = append(segments, segment)
			}
			start = i + 1
		}
		if current == '$' && i+1 < len(runes) && runes[i+1] == '(' {
			compound = true
		}
	}
	if segment := strings.TrimSpace(string(runes[start:])); segment != "" {
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		segments = []string{strings.TrimSpace(command)}
	}
	return segments, compound || quote != 0 || escaped
}

func invokesShellInterpreter(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	switch filepathBase(fields[0]) {
	case "sh", "bash", "dash", "zsh", "ksh", "fish":
		return true
	default:
		return false
	}
}

func filepathBase(path string) string {
	if index := strings.LastIndexAny(path, `/\\`); index >= 0 {
		return path[index+1:]
	}
	return path
}

func matchesAnyRegex(patterns []*regexp.Regexp, value string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}
