package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acceptedUndoFixture(t *testing.T) (context.Context, *Manager, string, Result) {
	t.Helper()
	ctx := context.Background()
	repo := newTestRepo(t)
	manager, err := New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: "slice", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(handle.Manifest.WorktreePath, "tracked.txt"), []byte("accepted\n"))
	result, err := manager.Integrate(ctx, handle, []string{"tracked.txt"})
	if err != nil || result.ReceiptID == "" {
		t.Fatalf("accepted slice = %+v, error = %v", result, err)
	}
	return ctx, manager, repo, result
}

func TestUndoAcceptedSlicePreservesDisjointUserWork(t *testing.T) {
	ctx, manager, repo, result := acceptedUndoFixture(t)
	mustWrite(t, filepath.Join(repo, "user.txt"), []byte("private bytes\n"))
	if err := manager.Undo(ctx, repo, result.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Undo(ctx, repo, result.ReceiptID); err != nil {
		t.Fatalf("repeating completed undo: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("restored parent = %q, error = %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "user.txt")); err != nil || string(data) != "private bytes\n" {
		t.Fatalf("disjoint user work = %q, error = %v", data, err)
	}
	receipt, err := manager.LoadReceipt(result.ReceiptID)
	if err != nil || receipt.State != "undone" {
		t.Fatalf("durable undo receipt = %+v, error = %v", receipt, err)
	}
}

func TestUndoRejectsConflictingUserEditWithoutChangingIt(t *testing.T) {
	ctx, manager, repo, result := acceptedUndoFixture(t)
	mustWrite(t, filepath.Join(repo, "tracked.txt"), []byte("user edit after acceptance\n"))
	if err := manager.Undo(ctx, repo, result.ReceiptID); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting undo = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(data) != "user edit after acceptance\n" {
		t.Fatalf("user edit after conflict = %q, error = %v", data, err)
	}
}

func TestUndoReconcilesReverseAppliedBeforeReceipt(t *testing.T) {
	ctx, manager, repo, result := acceptedUndoFixture(t)
	receipt, err := manager.LoadReceipt(result.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	receipt.State = "undo_prepared"
	if err := manager.writeReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	patch, err := manager.LoadArtifact(ctx, receipt.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.runner.Run(ctx, Command{Dir: repo, Args: []string{"apply", "-R", "--binary", "-"}, Stdin: patch}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Undo(ctx, repo, result.ReceiptID); err != nil {
		t.Fatalf("recover interrupted undo without reversing twice: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("reconciled undo = %q, error = %v", data, err)
	}
}
