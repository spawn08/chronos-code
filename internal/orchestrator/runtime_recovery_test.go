package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func TestInteractiveJournalRecoversEffectWithoutReplaying(t *testing.T) {
	root := t.TempDir()
	a := pipelineAgent(t)
	j := &interactiveJournal{a: a, root: root}
	ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "s1"), agent.RunIdentity{InvocationID: "old"})
	args := `{"path":"written.txt","content":"finished"}`
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "write the file"}}}
	resp := &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "effect-1", Name: "file_write", Arguments: args}}}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: resp}); err != nil {
		t.Fatal(err)
	}
	round, err := j.load(ctx)
	if err != nil || len(round.Calls) != 1 || round.Calls[0].State != "planned" {
		t.Fatalf("intent not durable: %+v %v", round, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatal(err)
	}
	// Model the crash window between destination mutation and receipt commit.
	round.Calls[0].State = "started"
	round.Calls[0].ExpectedHash = expectedWriteHash(ctx, root, parsed)
	round.Calls[0].ExpectedPath = "written.txt"
	if err := j.save(ctx, round); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "written.txt"), []byte("finished"), 0o600); err != nil {
		t.Fatal(err)
	}
	newCtx := agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "new"})
	recovered := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "policy"}, {Role: model.RoleUser, Content: "continue"}}}
	if err := (&interactiveJournal{a: a, root: root}).Before(newCtx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: recovered}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range recovered.Messages {
		if msg.ToolCallID == "effect-1" && strings.Contains(msg.Content, "recovered") {
			found = true
		}
	}
	if !found || recovered.Messages[len(recovered.Messages)-1].Content != "continue" {
		t.Fatalf("recovery lost effect or live request: %+v", recovered.Messages)
	}
	data, _ := os.ReadFile(filepath.Join(root, "written.txt"))
	if string(data) != "finished" {
		t.Fatal("recovery replayed or lost the destination")
	}
}

func TestSupervisorYieldsAndPreservesResultAcrossHosts(t *testing.T) {
	a := pipelineAgent(t)
	s := &toolSupervisor{runs: map[string]*supervisedTool{}, active: map[string]string{}, gate: make(chan struct{}, 1), yield: time.Millisecond, store: a.Storage}
	defer s.Close()
	ctx := storage.WithSession(context.Background(), "s1")
	release := make(chan struct{})
	var calls atomic.Int32
	handler := func(context.Context, map[string]any) (any, error) {
		calls.Add(1)
		<-release
		return map[string]any{"finding": "late finding"}, nil
	}
	out, err := s.start(ctx, "mcp__fixture__slow", map[string]any{"x": "y"}, handler)
	if err != nil {
		t.Fatal(err)
	}
	run := out.(map[string]any)
	if run["status"] != "running" {
		t.Fatalf("result=%v", run)
	}
	if _, err := s.start(ctx, "mcp__fixture__slow", map[string]any{"x": "y"}, handler); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate effect started")
	}
	if _, err := s.start(ctx, "mcp__fixture__other", nil, handler); err == nil {
		t.Fatal("capacity was not bounded")
	}
	close(release)
	id := run["tool_run_id"].(string)
	result, err := s.await(ctx, id, time.Second, false)
	if err != nil || result.(map[string]any)["status"] != "completed" {
		t.Fatalf("await=%v err=%v", result, err)
	}
	other := &toolSupervisor{runs: map[string]*supervisedTool{}, store: a.Storage}
	result, err = other.await(ctx, id, 0, false)
	if err != nil || !strings.Contains(errorFreeJSON(result), "late finding") {
		t.Fatalf("restart result=%v err=%v", result, err)
	}
	if _, err := other.await(storage.WithSession(ctx, "s2"), id, 0, false); err == nil {
		t.Fatal("another session obtained result")
	}
}

func errorFreeJSON(value any) string { data, _ := json.Marshal(value); return string(data) }

func TestDeferredSchemasKeepDispatchPermissions(t *testing.T) {
	root := t.TempDir()
	a := pipelineAgent(t)
	a.Tools.Register(builtins.NewFileReadTool(root))
	a.Tools.Register(&tool.Definition{Name: "mcp__test__denied", Permission: tool.PermDeny, Handler: func(context.Context, map[string]any) (any, error) { t.Fatal("denied tool ran"); return nil, nil }})
	for i := range 40 {
		a.Tools.Register(&tool.Definition{Name: "mcp__test__tool" + strings.Repeat("x", i), Permission: tool.PermAllow, Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, map[string]any) (any, error) { return "ok", nil }})
	}
	a.Hooks = append(a.Hooks, security.NewGuard(&security.Policy{DeniedPaths: []string{"**/.env"}}, root, nil))
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{DeferredSchemas: true, CoreTools: []string{"file_read"}}}}
	o.installDeferredTools(a)
	req := &model.ChatRequest{Tools: []model.ToolDefinition{{Function: model.FunctionDef{Name: "file_read"}}, {Function: model.FunctionDef{Name: "mcp__test__tool"}}}}
	if err := a.Hooks.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "file_read" {
		t.Fatalf("schemas=%v", req.Tools)
	}
	dispatch, _ := a.Tools.Get("call_tool")
	if _, err := dispatch.Handler(context.Background(), map[string]any{"name": "mcp__test__denied", "arguments": map[string]any{}}); err == nil {
		t.Fatal("dispatch bypassed denial")
	}
	if _, err := dispatch.Handler(context.Background(), map[string]any{"name": "file_read", "arguments": map[string]any{"path": ".env"}}); err == nil {
		t.Fatal("dispatch bypassed file guard")
	}
	out, err := dispatch.Handler(context.Background(), map[string]any{"name": "mcp__test__tool", "arguments": map[string]any{}})
	if err != nil || out != "ok" {
		t.Fatalf("dispatch=%v %v", out, err)
	}
}

func TestInteractiveJournalHostCrash(t *testing.T) {
	const childRoot = "CHRONOS_INTERACTIVE_JOURNAL_CRASH_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		store, err := sqlite.New(filepath.Join(root, "journal.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		a := &agent.Agent{ID: "crash-fixture", Storage: store}
		j := &interactiveJournal{a: a, root: root}
		ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "host-crash"), agent.RunIdentity{InvocationID: "terminated-host"})
		if err := j.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "effect.txt", "content": "exactly once"}}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "effect.txt"), []byte("exactly once"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("effect-written\n")
		select {}
	}

	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInteractiveJournalHostCrash$")
	cmd.Env = append(os.Environ(), childRoot+"="+root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "effect-written" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child exited before its effect")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach crash window")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	before, err := os.Stat(filepath.Join(root, "effect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.New(filepath.Join(root, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &agent.Agent{ID: "crash-fixture", Storage: store}
	j := &interactiveJournal{a: a, root: root}
	ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "host-crash"), agent.RunIdentity{InvocationID: "resumed-host"})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "continue"}}}
	if err := j.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	round, err := j.load(ctx)
	if err != nil || len(round.Calls) != 1 || round.Calls[0].State != "reconciled" {
		t.Fatalf("crashed effect not reconciled: %+v %v", round, err)
	}
	if err := j.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "effect.txt", "content": "exactly once"}}); err == nil {
		t.Fatal("resumed host permitted duplicate effect")
	}
	after, err := os.Stat(filepath.Join(root, "effect.txt"))
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("recovery rewrote destination: %v", err)
	}
}
