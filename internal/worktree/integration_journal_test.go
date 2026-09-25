package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type integrationFaultRunner struct {
	applyCalls  int
	failCleanup bool
}

func (r *integrationFaultRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if len(command.Args) > 1 && command.Args[0] == "apply" && command.Args[1] == "--binary" {
		r.applyCalls++
	}
	if r.failCleanup && len(command.Args) > 1 && command.Args[0] == "worktree" && command.Args[1] == "remove" {
		return CommandResult{}, fmt.Errorf("injected cleanup failure")
	}
	return ExecRunner{}.Run(ctx, command)
}

func TestIntegrationJournalRecoversAppliedPatchWithoutDuplicateApply(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	runner := &integrationFaultRunner{failCleanup: true}
	data := filepath.Join(t.TempDir(), "data")
	manager, err := New(data, runner)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: "attempt", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(handle.Manifest.WorktreePath, "tracked.txt"), []byte("accepted\n"))
	first, err := manager.Integrate(ctx, handle, []string{"tracked.txt"})
	if err == nil || first.ArtifactID == "" || first.Cleanup.State != string(CleanupPending) || runner.applyCalls != 1 {
		t.Fatalf("first integration = %+v, error = %v, apply calls = %d", first, err, runner.applyCalls)
	}
	manifest, err := readManifest(handle.Manifest.ManifestPath)
	if err != nil || manifest.Integration == nil || manifest.Integration.State != "applied" {
		t.Fatalf("retained integration = %+v, error = %v", manifest.Integration, err)
	}
	// Simulate a crash after git apply but before persisting the applied receipt.
	manifest.Integration.State = "prepared"
	if err := manager.persist(manifest); err != nil {
		t.Fatal(err)
	}
	runner.failCleanup = false
	restarted, err := New(data, runner)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Recover()
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovered integration = %+v, error = %v", recovered, err)
	}
	second, err := restarted.Integrate(ctx, recovered[0], []string{"tracked.txt"})
	if err != nil || second.Cleanup.State != "complete" || second.ArtifactID != first.ArtifactID || runner.applyCalls != 1 {
		t.Fatalf("reconciled integration = %+v, error = %v, apply calls = %d", second, err, runner.applyCalls)
	}
	content, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil || string(content) != "accepted\n" {
		t.Fatalf("parent content = %q, error = %v", content, err)
	}
}

func TestIntegrationJournalParksChangedParentWithoutReapplying(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	runner := &integrationFaultRunner{failCleanup: true}
	manager, err := New(filepath.Join(t.TempDir(), "data"), runner)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: "attempt", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(handle.Manifest.WorktreePath, "tracked.txt"), []byte("accepted\n"))
	if _, err := manager.Integrate(ctx, handle, []string{"tracked.txt"}); err == nil {
		t.Fatal("expected cleanup failure")
	}
	manifest, err := readManifest(handle.Manifest.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Integration.State = "prepared"
	if err := manager.persist(manifest); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "tracked.txt"), []byte("user changed after patch\n"))
	runner.failCleanup = false
	result, err := manager.Integrate(ctx, handle, []string{"tracked.txt"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || result.Cleanup.State != string(CleanupPending) || runner.applyCalls != 1 {
		t.Fatalf("ambiguous integration = %+v, error = %v, apply calls = %d", result, err, runner.applyCalls)
	}
	if err := manager.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if !fileExists(manifest.ManifestPath) {
		t.Fatal("prune discarded unresolved integration")
	}
}

func TestIndependentManagersComposeDisjointParentChanges(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	data := filepath.Join(t.TempDir(), "data")
	managers := make([]*Manager, 2)
	handles := make([]Handle, 2)
	for i, name := range []string{"first.txt", "second.txt"} {
		manager, err := New(data, nil)
		if err != nil {
			t.Fatal(err)
		}
		managers[i] = manager
		handles[i], err = manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: name, DirtyPolicy: DirtyReject})
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(handles[i].Manifest.WorktreePath, name), []byte(name))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, name := range []string{"first.txt", "second.txt"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, err := managers[i].Integrate(ctx, handles[i], []string{name})
			errs <- err
		}(i, name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"first.txt", "second.txt"} {
		if data, err := os.ReadFile(filepath.Join(repo, name)); err != nil || string(data) != name {
			t.Fatalf("composed %s = %q, error = %v", name, data, err)
		}
	}
}
