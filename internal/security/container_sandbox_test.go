package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
)

func TestMandatoryContainerSandboxRejectsUnpinnedOrUnavailableImage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tool := NewWorkspaceShellTool(root, time.Second)
	for _, policy := range []SandboxPolicy{{}, {Image: "alpine:latest"}, {Image: "alpine@sha256:" + strings.Repeat("f", 64), SocketPath: filepath.Join(root, "missing.sock")}} {
		_, err := tool.Handler(WithMandatorySandbox(ctx, policy), map[string]any{"command": "touch should-not-exist"})
		if !errors.Is(err, ErrMandatorySandboxUnavailable) {
			t.Fatalf("policy %+v error = %v", policy, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("unavailable sandbox executed an effect: %v", err)
	}
	shortRoot, err := os.MkdirTemp(os.TempDir(), "cc-") // Unix socket paths are length-limited on macOS.
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shortRoot)
	socketPath := filepath.Join(shortRoot, "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, err = NewContainerShellSandbox(ctx, shortRoot, SandboxPolicy{Image: "alpine@sha256:" + strings.Repeat("f", 64), SocketPath: socketPath})
	if !errors.Is(err, ErrMandatorySandboxUnavailable) {
		t.Fatalf("workspace-mounted Docker socket accepted: %v", err)
	}
}

func TestMandatoryContainerSandboxEnforcesWorkspaceSecretsAndNetwork(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("unattended container isolation is validated only on macOS")
	}
	image := os.Getenv("CHRONOS_CODE_SANDBOX_TEST_IMAGE")
	if image == "" {
		t.Skip("set CHRONOS_CODE_SANDBOX_TEST_IMAGE to a locally available pinned image to run Docker isolation gate")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible"), []byte("in-scope"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("out-of-scope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_TEST_HOST_SECRET", "no-container-access")
	ctx := WithMandatorySandbox(context.Background(), SandboxPolicy{Image: image})
	shellTool := NewWorkspaceShellTool(root, 15*time.Second)
	registry := tool.NewRegistry()
	shellTool.Permission = tool.PermAllow
	registry.Register(shellTool)
	if _, err := registry.Execute(WithEffectGrant(ctx, EffectRead), "shell", map[string]any{"command": "printf denied > should-not-exist"}); err == nil {
		t.Fatal("read-only grant ran an effectful shell")
	}
	ctx = WithEffectGrant(ctx, EffectRead, EffectScratchWrite, EffectDeliveryWrite, EffectProcessExecution, EffectNetwork, EffectExternalMutation)
	for _, test := range []struct {
		name, command string
		wantSuccess   bool
	}{
		{"workspace read", "cat visible", true},
		{"workspace write", "printf ok > output", true},
		{"outside read", fmt.Sprintf("cat %q", secret), false},
		{"symlink escape", "cat escape", false},
		{"outside write", fmt.Sprintf("printf bad > %q", filepath.Join(outside, "denied")), false},
		{"network", "wget -qO- -T 2 http://1.1.1.1", false},
		{"environment", "env", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := registry.Execute(ctx, "shell", map[string]any{"command": test.command})
			if err != nil {
				t.Fatalf("sandbox execution error: %v", err)
			}
			values := result.(map[string]any)
			if (values["exit_code"].(int) == 0) != test.wantSuccess {
				t.Fatalf("command %q result = %#v", test.command, values)
			}
			if strings.Contains(fmt.Sprint(values["stdout"], values["stderr"]), "no-container-access") {
				t.Fatal("host credential environment leaked into container")
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(root, "output"))
	if err != nil || string(data) != "ok" {
		t.Fatalf("workspace output = %q, error = %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "denied")); !os.IsNotExist(err) {
		t.Fatalf("sandbox wrote outside workspace: %v", err)
	}
}
