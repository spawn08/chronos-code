package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type runnerFunc func(context.Context, Command) (CommandResult, error)

func (f runnerFunc) Run(ctx context.Context, command Command) (CommandResult, error) {
	return f(ctx, command)
}

func TestCreatePersistsManifestBeforeWorktreeAdd(t *testing.T) {
	dataDir := t.TempDir()
	repo := t.TempDir()
	calls := 0
	runner := runnerFunc(func(_ context.Context, command Command) (CommandResult, error) {
		calls++
		switch calls {
		case 1:
			return CommandResult{Stdout: []byte(repo + "\n")}, nil
		case 2:
			return CommandResult{Stdout: []byte(strings.Repeat("a", 40) + "\n")}, nil
		case 3:
			return CommandResult{}, nil
		case 4:
			if !reflect.DeepEqual(command.Args[:2], []string{"worktree", "add"}) {
				t.Fatalf("worktree command = %v", command.Args)
			}
			entries, err := os.ReadDir(filepath.Join(dataDir, "worktrees", "manifests"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("manifest was not durable before worktree add: entries=%v err=%v", entries, err)
			}
			return CommandResult{}, errors.New("simulated crash")
		default:
			t.Fatalf("unexpected git call %d: %v", calls, command.Args)
			return CommandResult{}, nil
		}
	})
	manager, err := New(dataDir, runner)
	if err != nil {
		t.Fatal(err)
	}
	manager.random = func(p []byte) error {
		for i := range p {
			p[i] = 7
		}
		return nil
	}

	handle, err := manager.Create(context.Background(), repo, CreateOptions{TaskID: "task/1", AttemptID: "try 1", DirtyPolicy: DirtyPreserve})
	if err == nil || !strings.Contains(err.Error(), "recovery manifest retained") {
		t.Fatalf("Create() error = %v", err)
	}
	if handle.Manifest.CleanupState != CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", handle.Manifest.CleanupState)
	}
	recovered, err := manager.Recover()
	if err != nil || len(recovered) != 1 || recovered[0].Manifest.TaskID != "task/1" {
		t.Fatalf("Recover() = %+v, %v", recovered, err)
	}
}

func TestCreateRejectsDirtyParentWithoutAllocating(t *testing.T) {
	repo := t.TempDir()
	runner := &scriptedRunner{results: []scriptedResult{
		{stdout: repo + "\n"},
		{stdout: strings.Repeat("b", 40) + "\n"},
		{stdout: " M user.txt\x00"},
	}}
	manager, err := New(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(context.Background(), repo, CreateOptions{TaskID: "t", AttemptID: "a", DirtyPolicy: DirtyReject})
	if err == nil || !strings.Contains(err.Error(), "parent worktree is dirty") {
		t.Fatalf("Create() error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("git calls = %d, want no worktree add", len(runner.commands))
	}
}

func TestParseStatusPathsIncludesBothRenameSides(t *testing.T) {
	got, err := parseStatusPaths([]byte("R  new name\x00old name\x00?? untracked\x00"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new name", "old name", "untracked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestManagerCloseCleansRecoverableHandles(t *testing.T) {
	repo := newTestRepo(t)
	manager, err := New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(context.Background(), repo, CreateOptions{TaskID: "task", AttemptID: "attempt", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fileExists(handle.Manifest.WorktreePath) || fileExists(handle.Manifest.ManifestPath) {
		t.Fatal("Close left a worktree or manifest")
	}
}

type scriptedResult struct {
	stdout string
	err    error
}

type scriptedRunner struct {
	commands []Command
	results  []scriptedResult
}

func (r *scriptedRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	r.commands = append(r.commands, command)
	if len(r.results) == 0 {
		return CommandResult{}, errors.New("unexpected command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return CommandResult{Stdout: []byte(result.stdout)}, result.err
}
