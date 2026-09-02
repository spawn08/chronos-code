package security

import "regexp"

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
	switch toolName {
	case "file_read", "file_write", "file_list", "file_glob", "file_grep":
		if c.guard.checkFileArgs(toolName, args) != nil {
			return Deny
		}
	case "shell", "shell_auto":
		if c.guard.checkShellArgs(args, toolName == "shell_auto") != nil {
			return Deny
		}
		command, _ := args["command"].(string)
		if matchesAnyRegex(c.policy.neverAllow, command) {
			return Deny
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

func matchesAnyRegex(patterns []*regexp.Regexp, value string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}
