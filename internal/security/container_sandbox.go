package security

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	chronossandbox "github.com/spawn08/chronos/sandbox"
)

// ContainerShellSandbox makes only the admitted workspace visible from the
// host. Its image filesystem is read-only; no host environment is inherited.
type ContainerShellSandbox struct {
	Workspace string
	Container *chronossandbox.ContainerSandbox
}

func (s *ContainerShellSandbox) Close() error { return s.Container.Close() }

// NewContainerShellSandbox checks a pinned local image and reachable daemon
// before admitting an unattended effect. It never pulls an image implicitly.
func NewContainerShellSandbox(ctx context.Context, workspace string, policy SandboxPolicy) (*ContainerShellSandbox, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("unverified container platform %s: %w", runtime.GOOS, ErrMandatorySandboxUnavailable)
	}
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	marker := strings.LastIndex(policy.Image, "@sha256:")
	if marker <= 0 || len(policy.Image[marker+len("@sha256:"):]) != 64 {
		return nil, ErrMandatorySandboxUnavailable
	}
	if _, err := hex.DecodeString(policy.Image[marker+len("@sha256:"):]); err != nil {
		return nil, fmt.Errorf("invalid sandbox image digest: %w", ErrMandatorySandboxUnavailable)
	}
	socket := policy.SocketPath
	if socket == "" {
		socket = os.Getenv("DOCKER_HOST")
		if strings.HasPrefix(socket, "unix://") {
			socket = strings.TrimPrefix(socket, "unix://")
		} else if socket != "" {
			return nil, fmt.Errorf("non-local Docker endpoint: %w", ErrMandatorySandboxUnavailable)
		}
	}
	if socket == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate Docker socket: %w", err)
		}
		socket = filepath.Join(home, ".docker", "run", "docker.sock")
		if _, err := os.Stat(socket); err != nil {
			socket = "/var/run/docker.sock"
		}
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("Docker socket unavailable: %w", ErrMandatorySandboxUnavailable)
	}
	canonicalSocket, err := filepath.EvalSymlinks(socket)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker socket: %w", err)
	}
	relativeSocket, err := filepath.Rel(canonical, canonicalSocket)
	if err != nil {
		return nil, fmt.Errorf("scope Docker socket: %w", err)
	}
	if relativeSocket == "." || (!filepath.IsAbs(relativeSocket) && relativeSocket != ".." && !strings.HasPrefix(relativeSocket, ".."+string(filepath.Separator))) {
		return nil, fmt.Errorf("Docker socket is inside the mounted workspace: %w", ErrMandatorySandboxUnavailable)
	}
	if os.Getuid() == 0 {
		return nil, fmt.Errorf("root-owned unattended workspace is unsupported: %w", ErrMandatorySandboxUnavailable)
	}
	networkMode := "none"
	if policy.AllowNetwork {
		networkMode = "bridge"
	}
	container := chronossandbox.NewContainerSandbox(chronossandbox.ContainerConfig{
		Image: policy.Image, SocketPath: socket, NetworkMode: networkMode,
		User:       strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Mounts:     []chronossandbox.BindMount{{Source: canonical, Target: "/workspace"}},
		WorkingDir: "/workspace", Env: []string{"PATH=/bin:/usr/bin", "HOME=/workspace", "TMPDIR=/tmp"},
	})
	if err := container.Preflight(ctx); err != nil {
		container.Close()
		return nil, fmt.Errorf("sandbox image preflight: %w", err)
	}
	return &ContainerShellSandbox{Workspace: canonical, Container: container}, nil
}
