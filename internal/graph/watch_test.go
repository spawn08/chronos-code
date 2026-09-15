package graph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func lifecycleStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func lifecycleWrite(t *testing.T, root, path, contents string) string {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLifecycleWarmReopenSkipsGoLoad(t *testing.T) {
	root := newTinyModule(t)
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if _, err := NewIndexer(s, root).IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh Store/Indexer handles must use persisted completion evidence.
	bin := t.TempDir()
	lifecycleWrite(t, bin, "go", "#!/bin/sh\nexit 97\n")
	if err := os.Chmod(filepath.Join(bin, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stats, err := NewIndexer(s, root).IndexAll(ctx)
	if err != nil || stats.Files != 0 || stats.Skipped != 2 {
		t.Fatalf("warm pass = %+v, %v", stats, err)
	}
	lifecycleWrite(t, root, "go.sum", "changed module inputs\n")
	if _, err := NewIndexer(s, root).IndexAll(ctx); err == nil {
		t.Fatal("go.sum change failed to invalidate warm fingerprint")
	}
}

func TestLifecyclePreservesUnsupportedBuildFacts(t *testing.T) {
	if len(SupportedTreeSitterExtensions()) != 0 {
		t.Skip("default build preservation test")
	}
	root := newTinyModule(t)
	s := lifecycleStore(t)
	ctx := context.Background()
	if err := s.ReplaceFile(ctx, FileReplacement{Path: "lib/retained.py", Package: "lib", Hash: "other-build", Symbols: []Symbol{{Name: "FromOtherBuild", Kind: KindFunc}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPackage(ctx, "lib", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIndexer(s, root).IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if hash, err := s.FileHash(ctx, "lib/retained.py"); err != nil || hash != "other-build" {
		t.Fatalf("unsupported non-Go facts were pruned: %q, %v", hash, err)
	}
	if syms, err := s.FindSymbols(ctx, "FromOtherBuild", ""); err != nil || len(syms) != 1 {
		t.Fatalf("unsupported symbols = %v, %v", syms, err)
	}
}

func TestLifecycleBurstAndDeletion(t *testing.T) {
	root := newTinyModule(t)
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	b := lifecycleWrite(t, root, "b.go", "package tiny\nfunc ChangedSibling() {}\n")
	w := &Watcher{ix: ix}
	w.reindex(ctx, []string{filepath.Join(root, "a.go"), b})
	if syms, err := s.FindSymbols(ctx, "ChangedSibling", ""); err != nil || len(syms) != 1 {
		t.Fatalf("changed sibling was skipped: %v, %v", syms, err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	w.reindex(ctx, []string{b})
	if syms, err := s.SymbolsInFile(ctx, b); err != nil || len(syms) != 0 {
		t.Fatalf("deleted file retained: %v, %v", syms, err)
	}
}

func TestLifecycleMixedNonGo(t *testing.T) {
	if len(SupportedTreeSitterExtensions()) == 0 {
		t.Skip("tree-sitter build required")
	}
	root := newTinyModule(t)
	s := lifecycleStore(t)
	ctx := context.Background()
	path := lifecycleWrite(t, root, "lib/mod.py", "def Target():\n    pass\ndef Caller():\n    Target()\n")
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.SymbolsInFile(ctx, "lib/mod.py")
	if err != nil || len(before) != 2 {
		t.Fatalf("non-Go facts = %v, %v", before, err)
	}
	if hash, err := s.FileHash(ctx, "lib/mod.py"); err != nil || hash == "" {
		t.Fatalf("non-Go hash = %q, %v", hash, err)
	}
	lifecycleWrite(t, root, "a.go", "package tiny\nfunc A() { _ = 1 }\n")
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.SymbolsInFile(ctx, "lib/mod.py")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("Go refresh replaced non-Go facts: %v, %v", after, err)
	}
	if got, err := s.CallersOf(ctx, "Target"); err != nil || !reflect.DeepEqual(got, []string{"Caller"}) {
		t.Fatalf("non-Go edges = %v, %v", got, err)
	}
	if n, e, err := IndexNonGoFile(ctx, s, root, path); err != nil || n != 0 || e != 0 {
		t.Fatalf("unchanged non-Go reparse: %d, %d, %v", n, e, err)
	}
	if err := os.Rename(path, filepath.Join(root, "lib/renamed.py")); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{ix: ix}
	w.reindex(ctx, []string{path, filepath.Join(root, "lib/renamed.py")})
	if syms, err := s.SymbolsInFile(ctx, "lib/mod.py"); err != nil || len(syms) != 0 {
		t.Fatalf("renamed source retained: %v, %v", syms, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	w.reindex(ctx, []string{filepath.Join(root, "lib")})
	if callers, err := s.CallersOf(ctx, "Target"); err != nil || len(callers) != 0 {
		t.Fatalf("deleted non-Go edges = %v, %v", callers, err)
	}
}

func TestLifecycleCanceledAndGitignore(t *testing.T) {
	root := newTinyModule(t)
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s, %v", output, err)
	}
	lifecycleWrite(t, root, ".gitignore", "ignored/\n*.py\n!keep.py\n")
	lifecycleWrite(t, root, "ignored/ignored.go", "package ignored\nfunc Hidden() {}\n")
	lifecycleWrite(t, root, "hidden.py", "def HiddenPython(): pass\n")
	lifecycleWrite(t, root, "keep.py", "def KeptPython(): pass\n")
	s := lifecycleStore(t)
	ix := NewIndexer(s, root)
	ctx := context.Background()
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if syms, err := s.FindSymbols(ctx, "Hidden", ""); err != nil || len(syms) != 0 {
		t.Fatalf("ignored Go indexed: %v, %v", syms, err)
	}
	if syms, err := s.FindSymbols(ctx, "HiddenPython", ""); err != nil || len(syms) != 0 {
		t.Fatalf("ignored Python indexed: %v, %v", syms, err)
	}
	if len(SupportedTreeSitterExtensions()) != 0 {
		if syms, err := s.FindSymbols(ctx, "KeptPython", ""); err != nil || len(syms) != 1 {
			t.Fatalf("negated ignore lost: %v, %v", syms, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ix.IndexAll(canceled); err == nil {
		t.Fatal("canceled full index succeeded")
	}
	if _, err := ix.IndexFile(canceled, filepath.Join(root, "a.go")); err == nil {
		t.Fatal("canceled warm file index succeeded")
	}
	if _, err := Watch(canceled, ix); err == nil {
		t.Fatal("canceled watch succeeded")
	}
	w, err := Watch(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch close did not terminate")
	}
}

func TestLifecycleNonGoRollback(t *testing.T) {
	if len(SupportedTreeSitterExtensions()) == 0 {
		t.Skip("tree-sitter build required")
	}
	root := t.TempDir()
	s := lifecycleStore(t)
	ctx := context.Background()
	lifecycleWrite(t, root, "a.py", "def Target(): pass\ndef Old(): Target()\n")
	if _, _, err := IndexNonGoFile(ctx, s, root, "a.py"); err != nil {
		t.Fatal(err)
	}
	before, err := s.SymbolsInFile(ctx, "a.py")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := s.FileHash(ctx, "a.py")
	if err != nil {
		t.Fatal(err)
	}
	lifecycleWrite(t, root, "a.py", "def Target(): pass\ndef New(): Target()\n")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_non_go BEFORE INSERT ON edges BEGIN SELECT RAISE(ABORT, 'non-Go failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IndexNonGoFile(ctx, s, root, "a.py"); err == nil {
		t.Fatal("expected atomic replacement failure")
	}
	after, err := s.SymbolsInFile(ctx, "a.py")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed replacement changed symbols: %v, %v", after, err)
	}
	if got, err := s.FileHash(ctx, "a.py"); err != nil || got != hash {
		t.Fatalf("failed replacement hash = %q, %v", got, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_non_go`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIndexer(s, root).IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if callers, err := s.CallersOf(ctx, "Target"); err != nil || !reflect.DeepEqual(callers, []string{"New"}) {
		t.Fatalf("retry callers = %v, %v", callers, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := IndexNonGoFile(canceled, s, root, "a.py"); err == nil {
		t.Fatal("canceled unchanged non-Go index succeeded")
	}
}

func TestLifecycleBuildConfigAndReverseImports(t *testing.T) {
	root := newTinyModule(t)
	s := lifecycleStore(t)
	ctx := context.Background()
	lifecycleWrite(t, root, "a.go", "package tiny\nimport \"tiny/dep\"\nfunc Caller() { dep.Target() }\n")
	lifecycleWrite(t, root, "dep/dep.go", "package dep\nfunc Target() {}\n")
	lifecycleWrite(t, root, "extra.go", "//go:build lifecycle_extra\n\npackage tiny\nfunc Extra() {}\n")
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleWrite(t, root, "dep/dep.go", "package dep\nvar Target = func() {}\n")
	if _, err := ix.IndexFile(ctx, filepath.Join(root, "dep/dep.go")); err != nil {
		t.Fatal(err)
	}
	if callers, err := s.CallersOf(ctx, "Target"); err != nil || len(callers) != 0 {
		t.Fatalf("unchanged reverse importer not invalidated: %v, %v", callers, err)
	}
	if syms, err := s.FindSymbols(ctx, "Extra", ""); err != nil || len(syms) != 0 {
		t.Fatalf("inactive build-tag source indexed: %v, %v", syms, err)
	}
	t.Setenv("GOFLAGS", "-tags=lifecycle_extra")
	if _, err := ix.IndexFile(ctx, filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	if syms, err := s.FindSymbols(ctx, "Extra", ""); err != nil || len(syms) != 1 {
		t.Fatalf("changed build config ignored: %v, %v", syms, err)
	}
	t.Setenv("GOFLAGS", "")
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if syms, err := s.FindSymbols(ctx, "Extra", ""); err != nil || len(syms) != 0 {
		t.Fatalf("disabled build source retained: %v, %v", syms, err)
	}
}

func TestLifecycleScopedLoadAndSiblingSemantics(t *testing.T) {
	root := newTinyModule(t)
	lifecycleWrite(t, root, "a.go", "package tiny\nfunc Target() {}\n")
	lifecycleWrite(t, root, "b.go", "package tiny\nfunc Caller() { Target() }\n")
	lifecycleWrite(t, root, "independent/other.go", "package independent\nfunc Other() {}\n")
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := s.SymbolsInFile(ctx, filepath.Join(root, "independent/other.go"))
	if err != nil {
		t.Fatal(err)
	}
	path := lifecycleWrite(t, root, "a.go", "package tiny\nvar Target = func() {}\n")
	// A new Indexer must plan from persisted imports/inputs, not a warm AST cache.
	stats, err := NewIndexer(s, root).IndexFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packages != 1 || stats.Files != 2 {
		t.Fatalf("scoped semantic pass = %+v; want one package and both changed facts", stats)
	}
	if callers, err := s.CallersOf(ctx, "Target"); err != nil || len(callers) != 0 {
		t.Fatalf("unchanged sibling kept stale call: %v, %v", callers, err)
	}
	got, err := s.SymbolsInFile(ctx, filepath.Join(root, "independent/other.go"))
	if err != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("unaffected package changed: %v, %v", got, err)
	}
}

func TestLifecycleScopedReverseClosureAndStructuralTypes(t *testing.T) {
	root := newTinyModule(t)
	for path, text := range map[string]string{
		"dep/dep.go":           "package dep\nfunc Target() {}\n",
		"mid/mid.go":           "package mid\nimport \"tiny/dep\"\nfunc Wrapper() { dep.Target() }\n",
		"a.go":                 "package tiny\nimport \"tiny/mid\"\nfunc Caller() { mid.Wrapper() }\n",
		"independent/other.go": "package independent\nfunc Other() {}\n",
		"contract/contract.go": "package contract\ntype Contract interface { Run() }\n",
		"impl/impl.go":         "package impl\ntype Concrete struct{}\nfunc (Concrete) Run() {}\n",
	} {
		lifecycleWrite(t, root, path, text)
	}
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	path := lifecycleWrite(t, root, "dep/dep.go", "package dep\nvar Target = func() {}\n")
	stats, err := ix.IndexFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packages != 3 {
		t.Fatalf("reverse closure loaded %d packages, want dep/mid/tiny only", stats.Packages)
	}
	if callers, err := s.CallersOf(ctx, "Target"); err != nil || len(callers) != 0 {
		t.Fatalf("stale reverse call: %v, %v", callers, err)
	}
	path = lifecycleWrite(t, root, "impl/impl.go", "package impl\ntype Concrete struct{}\nfunc (Concrete) Run() { _ = 1 }\n")
	stats, err = ix.IndexFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packages != 1 {
		t.Fatalf("method-body-only edit loaded %d packages, want 1", stats.Packages)
	}
	if impls, err := s.ImplementationsOf(ctx, "Contract"); err != nil || !reflect.DeepEqual(impls, []string{"Concrete"}) {
		t.Fatalf("unloaded interface relationship lost: %v, %v", impls, err)
	}
	lifecycleWrite(t, root, "impl/impl.go", "package impl\ntype Concrete struct{}\nfunc (Concrete) Stop() {}\n")
	stats, err = ix.IndexFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packages != 2 {
		t.Fatalf("method-set edit loaded %d packages, want impl and contract", stats.Packages)
	}
	if impls, err := s.ImplementationsOf(ctx, "Contract"); err != nil || len(impls) != 0 {
		t.Fatalf("stale cross-package implementation: %v, %v", impls, err)
	}
	path = lifecycleWrite(t, root, "contract/contract.go", "package contract\ntype Contract interface { Stop() }\n")
	if _, err := ix.IndexFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if impls, err := s.ImplementationsOf(ctx, "Contract"); err != nil || !reflect.DeepEqual(impls, []string{"Concrete"}) {
		t.Fatalf("new independent implementation missed: %v, %v", impls, err)
	}
}

func TestLifecycleDeletedMethodInvalidatesIndependentInterface(t *testing.T) {
	root := newTinyModule(t)
	lifecycleWrite(t, root, "a.go", "package tiny\ntype Concrete struct{}\n")
	method := lifecycleWrite(t, root, "method.go", "package tiny\nfunc (Concrete) Run() {}\n")
	lifecycleWrite(t, root, "contract/contract.go", "package contract\ntype Contract interface { Run() }\n")
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(method); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, method); err != nil {
		t.Fatal(err)
	}
	if impls, err := s.ImplementationsOf(ctx, "Contract"); err != nil || len(impls) != 0 {
		t.Fatalf("deleted method retained structural edge: %v, %v", impls, err)
	}
}

func TestLifecycleNonGoEditAvoidsGoLoader(t *testing.T) {
	if len(SupportedTreeSitterExtensions()) == 0 {
		t.Skip("tree-sitter build required")
	}
	root := newTinyModule(t)
	lifecycleWrite(t, root, "mod.py", "def Before(): pass\n")
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := lifecycleWrite(t, bin, "go", "#!/bin/sh\nexit 97\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := lifecycleWrite(t, root, "mod.py", "def After(): pass\n")
	stats, err := ix.IndexFile(ctx, path)
	if err != nil || stats.Packages != 0 || stats.Files != 1 {
		t.Fatalf("non-Go-only edit used Go loader: %+v, %v", stats, err)
	}
	if syms, err := s.FindSymbols(ctx, "After", ""); err != nil || len(syms) != 1 {
		t.Fatalf("non-Go replacement missing: %v, %v", syms, err)
	}
}

func TestLifecycleCancelDuringLoad(t *testing.T) {
	root := newTinyModule(t)
	s := lifecycleStore(t)
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	script := lifecycleWrite(t, bin, "go", "#!/bin/sh\n: > \""+started+"\"\nexec sleep 30\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := NewIndexer(s, root).IndexAll(ctx); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Go load never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled load succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("load cancellation did not terminate")
	}
}

func TestLifecycleLiveWatchDirectoryRename(t *testing.T) {
	root := newTinyModule(t)
	s := lifecycleStore(t)
	ctx := context.Background()
	ix := NewIndexer(s, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := Watch(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	waitSymbol := func(name string, want int) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for {
			syms, err := s.FindSymbols(ctx, name, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(syms) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("watch symbol %s count = %d, want %d", name, len(syms), want)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	lifecycleWrite(t, root, "newpkg/new.go", "package newpkg\nfunc FromNewDirectory() {}\n")
	waitSymbol("FromNewDirectory", 1)
	if err := os.Rename(filepath.Join(root, "newpkg"), filepath.Join(root, "renamed")); err != nil {
		t.Fatal(err)
	}
	lifecycleWrite(t, root, "renamed/new.go", "package newpkg\nfunc FromRenamedDirectory() {}\n")
	waitSymbol("FromRenamedDirectory", 1)
	waitSymbol("FromNewDirectory", 0)
	if len(SupportedTreeSitterExtensions()) != 0 {
		path := lifecycleWrite(t, root, "mod.py", "def LivePython(): pass\n")
		waitSymbol("LivePython", 1)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		waitSymbol("LivePython", 0)
	}
	if err := os.RemoveAll(filepath.Join(root, "renamed")); err != nil {
		t.Fatal(err)
	}
	waitSymbol("FromRenamedDirectory", 0)
}
