package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSandboxCanonicalWorkspace(t *testing.T) {
	realWorkspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(realWorkspace, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	sb, err := newOSSandbox(link, false, "darwin", func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	if err != nil {
		t.Fatalf("newOSSandbox: %v", err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(realWorkspace)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if sb.workspace != wantWorkspace {
		t.Fatalf("workspace = %q, want canonical path %q", sb.workspace, wantWorkspace)
	}
}

func TestSandboxMacOSProfile(t *testing.T) {
	workspace := `/tmp/work space/quote"and\slash`
	sb := &OSSandbox{workspace: workspace, platform: "darwin"}
	args, err := sb.commandArgs("printf", []string{"%s", "hello world"})
	if err != nil {
		t.Fatalf("commandArgs: %v", err)
	}
	if len(args) != 6 || args[0] != "-p" || args[2] != "--" {
		t.Fatalf("args = %#v", args)
	}
	profile := args[1]
	if !strings.Contains(profile, `(allow file-write* (subpath "/tmp/work space/quote\"and\\slash"))`) {
		t.Fatalf("profile does not safely quote workspace:\n%s", profile)
	}
	if strings.Contains(profile, "allow network") {
		t.Fatalf("default profile allows network:\n%s", profile)
	}
	if strings.Contains(profile, "allow file-write*") && !strings.Contains(profile, "(subpath ") {
		t.Fatalf("profile contains an unrestricted write rule:\n%s", profile)
	}

	networkSB := &OSSandbox{workspace: workspace, platform: "darwin", allowNetwork: true}
	networkArgs, err := networkSB.commandArgs("true", nil)
	if err != nil {
		t.Fatalf("network commandArgs: %v", err)
	}
	if !strings.Contains(networkArgs[1], "(allow network*)") {
		t.Fatalf("network opt-in is absent:\n%s", networkArgs[1])
	}
}

func TestSandboxLinuxArgs(t *testing.T) {
	sb := &OSSandbox{workspace: "/work space", platform: "linux"}
	got, err := sb.commandArgs("sh", []string{"-c", "printf ok"})
	if err != nil {
		t.Fatalf("commandArgs: %v", err)
	}
	want := []string{
		"--die-with-parent", "--new-session", "--unshare-all",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--bind", "/work space", "/work space",
		"--chdir", "/work space",
		"--", "sh", "-c", "printf ok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}

	sb.allowNetwork = true
	got, err = sb.commandArgs("true", nil)
	if err != nil {
		t.Fatalf("network commandArgs: %v", err)
	}
	if !reflect.DeepEqual(got[:4], []string{"--die-with-parent", "--new-session", "--unshare-all", "--share-net"}) {
		t.Fatalf("network opt-in args = %#v", got)
	}
}

func TestSandboxMissingHelperFailsClosed(t *testing.T) {
	lookupErr := errors.New("not found")
	_, err := newOSSandbox(t.TempDir(), false, "linux", func(name string) (string, error) {
		if name != linuxSandboxHelper {
			t.Fatalf("helper = %q, want %q", name, linuxSandboxHelper)
		}
		return "", lookupErr
	})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want lookup failure", err)
	}
}

func TestSandboxMacOSWriteBoundary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is only available on macOS")
	}

	workspace := t.TempDir()
	sb, err := NewOSSandbox(workspace, false)
	if err != nil {
		t.Fatalf("NewOSSandbox: %v", err)
	}
	result, err := sb.Execute(context.Background(), "/usr/bin/touch", []string{"inside"}, 5*time.Second)
	if err != nil {
		t.Fatalf("workspace write: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("workspace write exit = %d, stderr = %q", result.ExitCode, result.Stderr)
	}

	outside := filepath.Join(filepath.Dir(sb.workspace), filepath.Base(sb.workspace)+"-outside")
	t.Cleanup(func() { _ = os.Remove(outside) })
	result, err = sb.Execute(context.Background(), "/usr/bin/touch", []string{outside}, 5*time.Second)
	if err != nil {
		t.Fatalf("outside write execution: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("outside write unexpectedly succeeded")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside path exists after denied write: %v", err)
	}
}

func TestSandboxUnsupportedPlatformFailsClosed(t *testing.T) {
	_, err := newOSSandbox(t.TempDir(), false, "windows", func(string) (string, error) {
		t.Fatal("helper lookup must not run on an unsupported platform")
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v, want unsupported-platform failure", err)
	}
}

func TestSandboxCancellation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "sandbox-exec")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 10\n"), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sb := &OSSandbox{
		workspace: t.TempDir(),
		helper:    script,
		platform:  "darwin",
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	_, err := sb.Execute(ctx, "true", nil, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestSandboxClose(t *testing.T) {
	if err := (&OSSandbox{}).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
