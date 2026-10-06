package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/engine/hooks"
)

func writePolicy(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorPolicyFileDeniesShellCommandsTheProjectCannotRestore(t *testing.T) {
	projectDir := t.TempDir()
	writePolicy(t, filepath.Join(projectDir, "security.yaml"), "shell:\n  never_allow: []\n  denied_patterns: []\n")
	operator := writePolicy(t, filepath.Join(t.TempDir(), "guard.yaml"), "shell:\n  never_allow: ['^(\\S*/)?(mvn|npm)(\\s|$)']\n")
	policy, err := ResolvePolicy(projectDir, t.TempDir(), operator)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	guard := security.NewGuard(policy, t.TempDir(), nil)
	shell := func(command string) error {
		return guard.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "shell", Input: map[string]any{"command": command}})
	}
	if err := shell("mvn -q package"); err == nil || !strings.Contains(err.Error(), "never_allow") {
		t.Fatalf("mvn = %v, want a never_allow denial", err)
	}
	if err := shell("git status"); err != nil {
		t.Fatalf("git status = %v, want allowed", err)
	}
}

func TestOperatorPolicyFileCannotWidenOrGrantTrust(t *testing.T) {
	for name, body := range map[string]string{
		"auto_allow":      "shell:\n  auto_allow: ['^curl ']\n",
		"trusted_digests": "hooks:\n  trusted_digests: ['abc']\n",
	} {
		t.Run(name, func(t *testing.T) {
			operator := writePolicy(t, filepath.Join(t.TempDir(), "guard.yaml"), body)
			_, err := ResolvePolicy(t.TempDir(), t.TempDir(), operator)
			if err == nil || !strings.Contains(err.Error(), "operator security overlay") {
				t.Fatalf("ResolvePolicy = %v, want an operator overlay error", err)
			}
		})
	}
	if _, err := ResolvePolicy(t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing operator policy file accepted")
	}
}
