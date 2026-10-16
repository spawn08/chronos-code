package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/verification"
)

func scopedRuntime(t *testing.T, files map[string]string) *taskRuntime {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := newTaskRuntime("scoped-task", root)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func scopedReceipt(t *testing.T, runtime *taskRuntime, command, directory string, revision uint64) {
	t.Helper()
	now := time.Now()
	if err := recordShellEvidence(runtime, context.Background(), map[string]any{"command": command, "working_dir": directory}, map[string]any{"exit_code": 0}, nil, now, now, revision); err != nil {
		t.Fatal(err)
	}
}

func TestScopedVerificationRequiresEveryManifestRoot(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"go.mod": "module example/root\n", "main.go": "package root\n", "nested/go.mod": "module example/nested\n", "nested/main.go": "package nested\n"})
	for _, path := range []string{"main.go", "nested/main.go"} {
		if _, err := runtime.recordWrite(path, "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	request := ExecutionRequest{VerificationMode: verification.ModeEnforce}
	scopedReceipt(t, runtime, "go test ./...", "", runtime.ledger.Revision())
	decision := assessRuntimeVerification(request, router.Classification{}, runtime)
	if decision.Allowed || len(decision.Obligations) != 2 {
		t.Fatalf("root command certified nested module: %#v", decision)
	}
	scopedReceipt(t, runtime, "cd nested && go test ./...", "", runtime.ledger.Revision())
	if decision = assessRuntimeVerification(request, router.Classification{}, runtime); !decision.Allowed {
		t.Fatalf("both modules checked: %#v", decision)
	}
}

func TestScopedVerificationPreservesSelectorsAndWorkingDirectory(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"go.mod": "module example/root\n", "a/a.go": "package a\n", "b/b.go": "package b\n"})
	for _, path := range []string{"a/a.go", "b/b.go"} {
		if _, err := runtime.recordWrite(path, "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	revision := runtime.ledger.Revision()
	scopedReceipt(t, runtime, "GOFLAGS='-run=TestOnly' go test ./...", "", revision)
	state, err := runtime.snapshot()
	if err != nil || len(state.Verification) != 0 || len(state.Uncertainties) != 1 {
		t.Fatalf("selector environment inferred broad coverage: %#v, %v", state, err)
	}
	scopedReceipt(t, runtime, "go test .", "a", revision)
	request := ExecutionRequest{VerificationMode: verification.ModeEnforce}
	if decision := assessRuntimeVerification(request, router.Classification{}, runtime); decision.Allowed {
		t.Fatalf("one package certified both: %#v", decision)
	}
	scopedReceipt(t, runtime, "go test .", "b", revision)
	if decision := assessRuntimeVerification(request, router.Classification{}, runtime); !decision.Allowed {
		t.Fatalf("both packages checked: %#v", decision)
	}
	if _, err := runtime.recordWrite("a/new.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err = runtime.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range state.Verification {
		if check.Event.Paths[0] == "a/a.go" && check.Current {
			t.Fatalf("new package member kept evidence current: %#v", check)
		}
		if check.Event.Paths[0] == "b/b.go" && !check.Current {
			t.Fatalf("unrelated package evidence became stale: %#v", check)
		}
	}
}

func TestBackgroundVerificationGateAndRepairAction(t *testing.T) {
	runtime := scopedRuntime(t, nil)
	runtime.trackBackground("writer", map[string]any{"command": "python generate.py"}, time.Now(), 0)
	runtime.trackBackground("reader", map[string]any{"command": "cat README.md"}, time.Now(), 0)
	for _, mode := range []verification.Mode{verification.ModeReport, verification.ModeEnforce} {
		decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: mode}, router.Classification{}, runtime)
		if !decision.Disagreement || decision.Allowed != (mode == verification.ModeReport) || len(decision.Obligations) != 1 {
			t.Fatalf("background gate: %#v", decision)
		}
		if action := obligationAction(decision.Obligations[0], nil); !strings.Contains(action, "await_shell") || !strings.Contains(action, "writer") {
			t.Fatalf("repair action: %s", action)
		}
	}
	runtime.finishBackground("writer")
	if decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: verification.ModeEnforce}, router.Classification{}, runtime); !decision.Allowed {
		t.Fatalf("read-only shell blocked completion: %#v", decision)
	}
}

func TestInheritedBackgroundRequiresQuiescence(t *testing.T) {
	runtime := scopedRuntime(t, nil)
	ctx := storage.WithSession(context.Background(), "background-session")
	runtime.observationCtx = ctx
	runtime.backgroundShells = security.NewBackgroundShells(t.TempDir())
	definition := security.NewBackgroundShellTool(runtime.workspaceRoot, time.Second, nil, runtime.backgroundShells)
	result, err := definition.Handler(ctx, map[string]any{"command": "sleep 2", "working_dir": runtime.workspaceRoot, "block_until_ms": 0})
	if err != nil {
		t.Fatal(err)
	}
	values, ok := result.(map[string]any)
	if !ok || values["status"] != "running" {
		t.Fatalf("background result: %#v", result)
	}
	id := values["shell_id"].(string)
	defer security.NewKillShellTool(runtime.backgroundShells).Handler(ctx, map[string]any{"shell_id": id})
	decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: verification.ModeEnforce}, router.Classification{}, runtime)
	if decision.Allowed || len(decision.Obligations) != 1 || decision.Obligations[0].ID != "background:"+id {
		t.Fatalf("untracked inherited process skipped: %#v", decision)
	}
}

func TestBackgroundExitScanStalesPriorCheckWithoutConsumingOutput(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"go.mod": "module example/root\n", "main.go": "package root\n"})
	ctx := storage.WithSession(context.Background(), "late-writer")
	runtime.observationCtx = ctx
	runtime.backgroundShells = security.NewBackgroundShells(t.TempDir())
	defer runtime.backgroundShells.Close()
	if err := runtime.refreshSourceChanges(true); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.recordWrite("main.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	scopedReceipt(t, runtime, "go test ./...", "", runtime.ledger.Revision())
	command := "sleep 0.05; printf 'package root\\n// late write\\n' > main.go; printf EXIT_MARKER"
	result, err := security.NewBackgroundShellTool(runtime.workspaceRoot, time.Second, nil, runtime.backgroundShells).Handler(ctx, map[string]any{"command": command, "block_until_ms": 0})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["shell_id"].(string)
	runtime.trackBackground(id, map[string]any{"command": command}, time.Now(), runtime.ledger.Revision())
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshots := runtime.backgroundShells.Snapshots(ctx)
		if len(snapshots) == 1 && snapshots[0].Exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background writer did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: verification.ModeEnforce}, router.Classification{}, runtime)
	if decision.Allowed || len(decision.Obligations) != 1 || decision.Obligations[0].Status != verification.StatusStale {
		t.Fatalf("late writer kept passing evidence: %#v", decision)
	}
	awaited, err := security.NewAwaitShellTool(runtime.backgroundShells).Handler(ctx, map[string]any{"shell_id": id})
	if err != nil || !strings.Contains(awaited.(map[string]any)["output"].(string), "EXIT_MARKER") {
		t.Fatalf("reconciliation consumed output: %#v, %v", awaited, err)
	}
}

func TestGoPackageRecheckCoversDeletedSibling(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"go.mod": "module example/root\n", "p/a.go": "package p\n", "p/b.go": "package p\n"})
	if err := os.Remove(filepath.Join(runtime.workspaceRoot, "p/b.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.recordWrite("p/b.go", "", 0, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	scopedReceipt(t, runtime, "go test ./p", "", runtime.ledger.Revision())
	if decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: verification.ModeEnforce}, router.Classification{}, runtime); !decision.Allowed {
		t.Fatalf("package recheck did not cover removed input: %#v", decision)
	}
}

func TestDevelopmentServersRemainPotentialBackgroundWriters(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"package.json": `{"scripts":{"dev":"vite","start":"next dev"}}`})
	for _, command := range []string{"npm run dev", "yarn start", "pnpm run dev", "bun run dev"} {
		if !runtime.backgroundMayChangeSource(map[string]any{"command": command}) {
			t.Errorf("server %q skipped completion gate", command)
		}
	}
}

func TestScopedShellPlanKeepsLiteralDirectoriesAndRejectsDynamicCD(t *testing.T) {
	steps := planShellEvidenceAt(`cd 'nested module' && CI=1 go test ./...`, "/workspace")
	if len(steps) != 1 || steps[0].directory != "/workspace/nested module" || steps[0].scopeCommand != "CI=1 go test ./..." {
		t.Fatalf("literal plan: %#v", steps)
	}
	for _, command := range []string{"cd missing; go test ./...", "cd $MODULE && go test ./...", "cd - && go test ./...", "cd nested | cat && go test ./..."} {
		steps := planShellEvidenceAt(command, "/workspace")
		if len(steps) != 1 || !steps[0].mutation {
			t.Fatalf("ambiguous cwd acquired evidence: %q: %#v", command, steps)
		}
	}
}

func TestPackageInputWriteDuringCheckCancelsOnlyAffectedEvidence(t *testing.T) {
	runtime := scopedRuntime(t, map[string]string{"go.mod": "module example/root\n", "a/a.go": "package a\n", "b/b.go": "package b\n"})
	revision := runtime.ledger.Revision()
	if _, err := runtime.recordWrite("a/extra.h", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	scopedReceipt(t, runtime, "go test ./a", "", revision)
	scopedReceipt(t, runtime, "go test ./b", "", revision)
	state, err := runtime.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range state.Verification {
		if check.Event.Paths[0] == "a/a.go" && (check.Event.Passed || check.Event.TerminalState != execution.TerminalCancelled) {
			t.Fatalf("concurrent package input accepted: %#v", check)
		}
		if check.Event.Paths[0] == "b/b.go" && !check.Event.Passed {
			t.Fatalf("unrelated package check rejected: %#v", check)
		}
	}
}

func TestSelectedPackageScriptsProduceMatchingReceipts(t *testing.T) {
	for _, script := range []struct {
		name, body string
		class      execution.CommandClass
	}{{"test", "jest", execution.CommandTest}, {"typecheck", "tsc --noEmit", execution.CommandDiagnostics}, {"check", "tsc --noEmit", execution.CommandDiagnostics}, {"lint", "eslint .", execution.CommandDiagnostics}, {"build", "vite build", execution.CommandBuild}} {
		for _, manager := range []string{"npm", "yarn", "pnpm", "bun"} {
			t.Run(manager+"/"+script.name, func(t *testing.T) {
				runtime := scopedRuntime(t, map[string]string{"package.json": `{"scripts":{"` + script.name + `":"` + script.body + `"}}`, "src/main.ts": "export const value = 1"})
				if _, err := runtime.recordWrite("src/main.ts", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
					t.Fatal(err)
				}
				command := manager + " run " + script.name
				if classify := classifyShellCommand(command); classify != script.class {
					t.Fatalf("class: %s, want %s", classify, script.class)
				}
				scopedReceipt(t, runtime, command, "", runtime.ledger.Revision())
				if decision := assessRuntimeVerification(ExecutionRequest{VerificationMode: verification.ModeEnforce}, router.Classification{}, runtime); !decision.Allowed {
					t.Fatalf("selected script could not satisfy check: %#v", decision)
				}
			})
		}
	}
}
