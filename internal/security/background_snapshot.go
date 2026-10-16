package security

import (
	"context"
	"sort"
	"time"

	"github.com/spawn08/chronos/storage"
)

// ShellSnapshot observes lifecycle state without consuming output or forgetting a shell.
type ShellSnapshot struct {
	ID          string
	Command     string
	Directory   string
	StartedAt   time.Time
	CompletedAt time.Time
	Exited      bool
	ExitCode    int
}

func (b *BackgroundShells) Snapshots(ctx context.Context) []ShellSnapshot {
	b.mu.Lock()
	var shells []*backgroundShell
	for _, sh := range b.shells {
		if sh.session == storage.SessionFromContext(ctx) && sh.tenant == storage.TenantFromContext(ctx) {
			shells = append(shells, sh)
		}
	}
	b.mu.Unlock()
	result := make([]ShellSnapshot, 0, len(shells))
	for _, sh := range shells {
		result = append(result, snapshotShell(sh))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func snapshotShell(sh *backgroundShell) ShellSnapshot {
	result := ShellSnapshot{ID: sh.id, Command: sh.command, Directory: sh.dir, StartedAt: sh.started}
	select {
	case <-sh.done:
		result.Exited, result.ExitCode, result.CompletedAt = true, sh.exitCode, sh.completed
	default:
	}
	return result
}
