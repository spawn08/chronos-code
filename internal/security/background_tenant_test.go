package security

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"
)

func TestBackgroundShellTenantIsolation(t *testing.T) {
	shells := NewBackgroundShells(t.TempDir())
	defer shells.Close()
	ctx := storage.WithTenant(storage.WithSession(context.Background(), "same-session"), "owner")
	out, err := NewBackgroundShellTool(t.TempDir(), time.Minute, nil, shells).Handler(ctx, map[string]any{"command": "sleep 30", "block_until_ms": 0})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["shell_id"].(string)
	other := storage.WithTenant(ctx, "other")
	if _, err := shells.await(other, id, nil, 0); err == nil {
		t.Fatal("another tenant awaited shell")
	}
	if _, err := shells.kill(other, id); err == nil {
		t.Fatal("another tenant cancelled shell")
	}
	if lines := shells.RunningContext(other); len(lines) != 0 {
		t.Fatalf("another tenant saw shell status: %v", lines)
	}
	if lines := shells.RunningContext(ctx); len(lines) != 1 {
		t.Fatalf("owner lost shell status: %v", lines)
	}
	if _, err := shells.ReadLog(other, id); err == nil {
		t.Fatal("another tenant read shell log")
	}
	if _, err := shells.kill(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundSnapshotsDoNotConsumeOrForgetShell(t *testing.T) {
	ctx := storage.WithTenant(storage.WithSession(context.Background(), "session"), "tenant")
	shells := NewBackgroundShells(t.TempDir())
	started := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	sh := &backgroundShell{
		id: "shell", tenant: "tenant", session: "session", command: "echo unread; exit 3",
		dir: t.TempDir(), started: started, completed: started.Add(time.Second), exitCode: 3,
		output: &shellOutput{}, done: make(chan struct{}), readCount: len("already read\n"),
	}
	if _, err := sh.output.Write([]byte("already read\nunread\n")); err != nil {
		t.Fatal(err)
	}
	close(sh.done)
	shells.shells[sh.id] = sh
	want := []ShellSnapshot{{ID: sh.id, Command: sh.command, Directory: sh.dir, StartedAt: started, CompletedAt: sh.completed, Exited: true, ExitCode: 3}}
	for range 3 {
		got := shells.Snapshots(ctx)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Snapshots() = %#v, want %#v", got, want)
		}
		got[0].Command = "caller mutation"
	}
	if sh.readCount != len("already read\n") {
		t.Fatalf("snapshot advanced read offset to %d", sh.readCount)
	}
	result, err := shells.await(ctx, sh.id, nil, time.Second)
	if err != nil {
		t.Fatalf("snapshot forgot exited shell: %v", err)
	}
	if result["output"] != "unread\n" || result["status"] != "exited" || result["exit_code"] != 3 {
		t.Fatalf("await after snapshots = %#v", result)
	}
	if got := shells.Snapshots(ctx); len(got) != 0 {
		t.Fatalf("Snapshots() after final await = %#v, want empty", got)
	}
}

func TestBackgroundSnapshotsFilterTenantAndSession(t *testing.T) {
	shells := NewBackgroundShells(t.TempDir())
	started := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, sh := range []*backgroundShell{
		{id: "z-owner", tenant: "tenant", session: "session"},
		{id: "a-owner", tenant: "tenant", session: "session"},
		{id: "other-tenant", tenant: "other", session: "session"},
		{id: "other-session", tenant: "tenant", session: "other"},
		{id: "default-owner", tenant: storage.TenantFromContext(context.Background()), session: storage.SessionFromContext(context.Background())},
	} {
		sh.started, sh.done = started, make(chan struct{})
		shells.shells[sh.id] = sh
	}
	for _, test := range []struct {
		name, tenant, session string
		want                  []string
	}{
		{name: "owner sorted", tenant: "tenant", session: "session", want: []string{"a-owner", "z-owner"}},
		{name: "other tenant", tenant: "other", session: "session", want: []string{"other-tenant"}},
		{name: "other session", tenant: "tenant", session: "other", want: []string{"other-session"}},
		{name: "no matching owner", tenant: "other", session: "other", want: []string{}},
		{name: "default owner", want: []string{"default-owner"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := storage.WithTenant(storage.WithSession(context.Background(), test.session), test.tenant)
			got := make([]string, 0)
			for _, snapshot := range shells.Snapshots(ctx) {
				got = append(got, snapshot.ID)
				if snapshot.Exited || !snapshot.CompletedAt.IsZero() || snapshot.ExitCode != 0 || snapshot.StartedAt != started {
					t.Fatalf("running snapshot = %#v", snapshot)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("snapshot IDs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBackgroundSnapshotsConcurrentCompletionAndAwait(t *testing.T) {
	ctx := storage.WithTenant(storage.WithSession(context.Background(), "session"), "tenant")
	shells := NewBackgroundShells(t.TempDir())
	started := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	const count = 64
	pending := make([]*backgroundShell, count)
	for i := range pending {
		sh := &backgroundShell{
			id: fmt.Sprintf("shell-%03d", i), tenant: "tenant", session: "session",
			started: started, output: &shellOutput{}, done: make(chan struct{}),
		}
		pending[i] = sh
		shells.shells[sh.id] = sh
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for range 256 {
				for _, snapshot := range shells.Snapshots(ctx) {
					if snapshot.Exited {
						if snapshot.ExitCode != 3 || snapshot.CompletedAt != started.Add(time.Second) {
							t.Errorf("incomplete exited snapshot: %#v", snapshot)
						}
					} else if snapshot.ExitCode != 0 || !snapshot.CompletedAt.IsZero() {
						t.Errorf("unpublished completion in running snapshot: %#v", snapshot)
					}
				}
			}
		}()
	}
	for _, sh := range pending {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if _, err := sh.output.Write([]byte("unread\n")); err != nil {
				t.Error(err)
			}
			sh.exitCode, sh.completed = 3, started.Add(time.Second)
			close(sh.done)
			result, err := shells.await(ctx, sh.id, nil, time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			if result["output"] != "unread\n" || result["exit_code"] != 3 {
				t.Errorf("concurrent await = %#v", result)
			}
		}()
	}
	close(start)
	group.Wait()
	if got := shells.Snapshots(ctx); len(got) != 0 {
		t.Fatalf("snapshots retained awaited shells: %#v", got)
	}
}
