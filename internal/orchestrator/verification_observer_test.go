package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
)

func TestVerificationObserverRecordsActualFileWrite(t *testing.T) {
	root := t.TempDir()
	runtime, err := newTaskRuntime("task", root)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	registry.Register(&tool.Definition{Name: "file_write", Handler: func(_ context.Context, args map[string]any) (any, error) {
		path := filepath.Join(root, args["path"].(string))
		content := []byte(args["content"].(string))
		return nil, os.WriteFile(path, content, 0o600)
	}})
	a := &agent.Agent{ID: "coder", Tools: registry}
	wrapVerificationEvidence(a)
	ctx := withTaskRuntime(context.Background(), runtime)
	if _, err := registry.Execute(ctx, "file_write", map[string]any{"path": "main.go", "content": "package main\n"}); err != nil {
		t.Fatal(err)
	}
	state, err := runtime.snapshot()
	if err != nil || len(state.Writes) != 1 || state.Writes[0].Paths[0] != "main.go" || state.Writes[0].ContentHash == "" {
		t.Fatalf("write evidence = %#v, %v", state.Writes, err)
	}
}

func TestVerificationObserverClassifiesShellResults(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	registry.Register(&tool.Definition{Name: "shell", Handler: func(context.Context, map[string]any) (any, error) {
		return map[string]any{"exit_code": 1}, nil
	}})
	a := &agent.Agent{ID: "coder", Tools: registry}
	wrapVerificationEvidence(a)
	ctx := withTaskRuntime(context.Background(), runtime)
	if _, err := registry.Execute(ctx, "shell", map[string]any{"command": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	state, err := runtime.snapshot()
	if err != nil || len(state.Verification) != 1 {
		t.Fatalf("verification evidence = %#v, %v", state.Verification, err)
	}
	for _, evidence := range state.Verification {
		if evidence.Event.CommandClass != execution.CommandTest || evidence.Event.Passed {
			t.Fatalf("shell evidence = %#v", evidence)
		}
	}
}

func TestClassifyShellCommandIsConservative(t *testing.T) {
	for command, want := range map[string]execution.CommandClass{
		"go test ./...":             execution.CommandTest,
		"go build ./...":            execution.CommandBuild,
		"go vet ./...":              execution.CommandDiagnostics,
		"git diff --check":          execution.CommandDiff,
		"git status --short":        execution.CommandRead,
		"go test ./... && rm -rf x": execution.CommandMutation,
		"custom-script":             execution.CommandUnknown,
	} {
		if got := classifyShellCommand(command); got != want {
			t.Errorf("classifyShellCommand(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestPlanShellEvidenceCountsCommonAgentCommands(t *testing.T) {
	type want struct {
		checks    []string
		mutations int
	}
	for command, expected := range map[string]want{
		"go test ./... 2>&1":                   {checks: []string{"test:go test ./..."}},
		"cd pkg && go test ./...":              {checks: []string{"test:go test ./..."}},
		"go build ./... && go test ./...":      {checks: []string{"build:go build ./...", "test:go test ./..."}},
		"GOFLAGS=-count=1 go test ./...":       {checks: []string{"test:go test ./..."}},
		"git diff --stat":                      {checks: []string{"diff:git diff --stat"}},
		"go test ./... 2>&1 | tail -30":        {},
		"cat main.go | head -20":               {},
		"grep -rn foo . > /dev/null":           {},
		"sed -n '1,20p' main.go":               {},
		"find . -name '*.go'":                  {},
		"go vet ./...; go test ./...":          {checks: []string{"test:go test ./..."}},
		"go test ./... > out.txt":              {mutations: 1, checks: []string{"test:go test ./..."}},
		"sed -i 's/a/b/' main.go":              {mutations: 1},
		"find . -name '*.tmp' -delete":         {mutations: 1},
		"go test ./... && rm -rf x":            {mutations: 1, checks: []string{"test:go test ./..."}},
		"echo $(rm -rf x)":                     {mutations: 1},
		"go test ./... | tee log && rm x":      {mutations: 2},
		"./build.sh":                           {mutations: 1},
		"go test -run 'TestA|TestB' ./pkg/...": {checks: []string{"test:go test -run 'TestA|TestB' ./pkg/..."}},
	} {
		var checks []string
		mutations := 0
		for _, step := range planShellEvidence(command) {
			if step.mutation {
				mutations++
				continue
			}
			checks = append(checks, string(step.class)+":"+step.command)
		}
		if mutations != expected.mutations || strings.Join(checks, ",") != strings.Join(expected.checks, ",") {
			t.Errorf("planShellEvidence(%q) = checks %v, mutations %d; want %v, %d", command, checks, mutations, expected.checks, expected.mutations)
		}
	}
}
