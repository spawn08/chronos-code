package security

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"

	"github.com/spawn08/chronos-code/internal/defaults"
)

func embeddedPolicy(t *testing.T, overlays ...Overlay) *Policy {
	t.Helper()
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		t.Fatalf("read embedded security floor: %v", err)
	}
	policy, err := ResolvePolicy(floor, overlays...)
	if err != nil {
		t.Fatalf("ResolvePolicy() error = %v", err)
	}
	return policy
}

func TestResolvePolicyMissingAndTighteningOverlaysPreserveFloor(t *testing.T) {
	floor := embeddedPolicy(t)
	effective := embeddedPolicy(t, Overlay{Source: "project", Data: []byte(`
filesystem:
  writable_paths: ["internal"]
  readable_paths: ["internal"]
  denied_paths: ["private/**"]
shell:
  allowed_commands: ["go", "git"]
  auto_allow: []
  confirm: ['^git status']
  never_allow: ['^shutdown']
  max_execution_time_sec: 30
secrets:
  patterns: ["CUSTOM_SECRET"]
mcp:
  denied_servers: ["postgres"]
  trusted_servers: ["filesystem"]
  default_permission: "deny"
  max_connections: 2
`)})

	for _, path := range floor.DeniedPaths {
		if !contains(effective.DeniedPaths, path) {
			t.Errorf("embedded denied path %q was removed", path)
		}
	}
	for _, pattern := range floor.neverAllowPatterns {
		if !contains(effective.neverAllowPatterns, pattern) {
			t.Errorf("embedded never_allow pattern was removed")
		}
	}
	if !contains(effective.DeniedPaths, "private/**") || effective.MaxExecSeconds != 30 {
		t.Fatalf("tightening overlay not applied: %#v", effective)
	}
	if got := effective.DecideMCPServer("postgres").Permission; got != MCPDeny {
		t.Fatalf("postgres decision = %q, want deny", got)
	}
	if got := effective.DecideMCPServer("filesystem").Permission; got != MCPAllow {
		t.Fatalf("filesystem decision = %q, want allow", got)
	}
	if got := effective.DecideMCPServer("other").Permission; got != MCPDeny {
		t.Fatalf("unknown decision = %q, want tightened default deny", got)
	}
	if effective.MaxMCPConnections != 2 {
		t.Fatalf("MaxMCPConnections = %d, want 2", effective.MaxMCPConnections)
	}
}

func TestResolvePolicyRejectsWeakeningOverlayFields(t *testing.T) {
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		overlay string
		field   string
	}{
		{"writable scope", "filesystem:\n  writable_paths: ['..']\n", "filesystem.writable_paths[0]"},
		{"readable scope", "filesystem:\n  readable_paths: ['/']\n", "filesystem.readable_paths[0]"},
		{"shell allowlist", "shell:\n  allowed_commands: ['bash']\n", "shell.allowed_commands[0]"},
		{"auto approval", "shell:\n  auto_allow: ['.*']\n", "shell.auto_allow[0]"},
		{"execution limit", "shell:\n  max_execution_time_sec: 301\n", "shell.max_execution_time_sec"},
		{"secret scanning", "secrets:\n  scan_output: false\n", "secrets.scan_output"},
		{"MCP trust", "mcp:\n  trusted_servers: ['untrusted']\n", "mcp.trusted_servers[0]"},
		{"MCP default", "mcp:\n  default_permission: allow\n", "mcp.default_permission"},
		{"MCP limit", "mcp:\n  max_connections: 6\n", "mcp.max_connections"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := ResolvePolicy(floor, Overlay{Source: "project", Data: []byte(tc.overlay)})
			if err == nil || policy != nil {
				t.Fatalf("ResolvePolicy() = %#v, %v; want nil policy and error", policy, err)
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "project security overlay") {
				t.Fatalf("error %q does not identify source and field %q", err, tc.field)
			}
		})
	}
}

func TestResolvePolicyRejectsMalformedOverlayWithoutReturningPolicy(t *testing.T) {
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, overlay := range []string{
		"filesystem: [",
		"shell:\n  never_allow: ['[']\n",
		"secrets:\n  patterns: ['[']\n",
		"shell:\n  unknown_rule: true\n",
		"filesystem: {}\n---\nshell: {}\n",
	} {
		policy, err := ResolvePolicy(floor, Overlay{Source: "user", Data: []byte(overlay)})
		if err == nil || policy != nil {
			t.Fatalf("ResolvePolicy(%q) = %#v, %v; want nil policy and error", overlay, policy, err)
		}
		if !strings.Contains(err.Error(), "user security overlay") {
			t.Fatalf("error %q lacks safe overlay source", err)
		}
	}
}

func TestResolvePolicyAppliesUserAndProjectOverlaysMonotonically(t *testing.T) {
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := ResolvePolicy(floor,
		Overlay{Source: "user", Data: []byte("filesystem:\n  writable_paths: ['internal']\nmcp:\n  max_connections: 4\n")},
		Overlay{Source: "project", Data: []byte("filesystem:\n  writable_paths: ['internal/security']\nmcp:\n  max_connections: 2\n")},
	)
	if err != nil {
		t.Fatalf("ResolvePolicy() error = %v", err)
	}
	if len(policy.WritablePaths) != 1 || policy.WritablePaths[0] != "internal/security" || policy.MaxMCPConnections != 2 {
		t.Fatalf("effective layered policy = %#v", policy)
	}

	policy, err = ResolvePolicy(floor,
		Overlay{Source: "user", Data: []byte("filesystem:\n  writable_paths: ['internal']\n")},
		Overlay{Source: "project", Data: []byte("filesystem:\n  writable_paths: ['.']\n")},
	)
	if err == nil || policy != nil || !strings.Contains(err.Error(), "project security overlay") {
		t.Fatalf("weakening project overlay = %#v, %v; want nil policy and project error", policy, err)
	}
}

func TestEmptyAllowedCommandsOverlayDoesNotBecomeAllowAll(t *testing.T) {
	policy := embeddedPolicy(t, Overlay{Source: "project", Data: []byte("shell:\n  allowed_commands: []\n")})
	checker := NewPermissionChecker(policy, "/workspace")
	if got := checker.Check("shell_auto", map[string]any{"command": "unlisted-command"}, true); got != Deny {
		t.Fatalf("empty allowlist shell_auto decision = %q, want deny", got)
	}
	if got := checker.Check("shell", map[string]any{"command": "unlisted-command"}, true); got != Confirm {
		t.Fatalf("empty allowlist shell decision = %q, want confirm", got)
	}
}

func TestResolvedFloorCannotBeBypassedByYolo(t *testing.T) {
	policy := embeddedPolicy(t, Overlay{Source: "project", Data: []byte("filesystem:\n  denied_paths: []\nshell:\n  never_allow: []\n")})
	checker := NewPermissionChecker(policy, "/workspace")
	if got := checker.Check("file_read", map[string]any{"path": ".env"}, true); got != Deny {
		t.Fatalf("yolo .env decision = %q, want deny", got)
	}
	if got := checker.Check("shell", map[string]any{"command": "sudo true"}, true); got != Deny {
		t.Fatalf("yolo sudo decision = %q, want deny", got)
	}
	if got := checker.Check("unregistered_tool", nil, true); got != Confirm {
		t.Fatalf("yolo unknown tool decision = %q, want confirm", got)
	}
}

func TestEmptyScopeOverlayDeniesAccess(t *testing.T) {
	policy := embeddedPolicy(t, Overlay{Source: "project", Data: []byte("filesystem:\n  writable_paths: []\n  readable_paths: []\n")})
	guard := NewGuard(policy, "/workspace", nil)
	for _, tc := range []struct {
		tool string
		path string
	}{
		{"file_read", "README.md"},
		{"file_write", "README.md"},
	} {
		err := guard.Before(context.Background(), &hooks.Event{
			Type:  hooks.EventToolCallBefore,
			Name:  tc.tool,
			Input: map[string]any{"path": tc.path},
		})
		if err == nil {
			t.Errorf("%s unexpectedly allowed with empty scope", tc.tool)
		}
	}
}

func TestMCPDecisionsUseSafeReasons(t *testing.T) {
	secretName := "server-token-super-secret"
	policy := embeddedPolicy(t, Overlay{Source: "project", Data: []byte("mcp:\n  denied_servers: ['" + secretName + "']\n")})
	for _, name := range []string{secretName, "filesystem", "unknown-password-value"} {
		decision := policy.DecideMCPServer(name)
		if strings.Contains(decision.Reason, name) || strings.Contains(decision.Reason, "super-secret") || strings.Contains(decision.Reason, "password") {
			t.Fatalf("decision reason %q contains server or secret-shaped value", decision.Reason)
		}
	}
	if got := policy.DecideMCPServer(secretName).Permission; got != MCPDeny {
		t.Fatalf("denied server decision = %q, want deny", got)
	}
	if got := policy.DecideMCPServer("unknown").Permission; got != MCPRequireApproval {
		t.Fatalf("unknown server decision = %q, want require_approval", got)
	}
}
