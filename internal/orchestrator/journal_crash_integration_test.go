package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func TestInteractiveJournalCompactionLeavesPendingEffectsRecoverable(t *testing.T) {
	a := pipelineAgent(t)
	journal := &interactiveJournal{a: a}
	ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "summary-pending"), agent.RunIdentity{InvocationID: "before"})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "apply an effect"}, {Role: model.RoleUser, Name: workingContextName, TurnScoped: true, Content: "old task state"}}}
	resp := &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "pending", Name: "fixture_effect", Arguments: `{}`}}}
	if err := journal.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: resp}); err != nil {
		t.Fatal(err)
	}
	resumed := agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "after"})
	summary := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a conversation summarizer. Preserve pending effects."}, {Role: model.RoleUser, Content: "history to summarize"}}}
	if err := journal.Before(resumed, &hooks.Event{Type: hooks.EventModelCallBefore, Input: summary}); err != nil {
		t.Fatal(err)
	}
	if len(summary.Messages) != 2 || summary.Messages[1].Content != "history to summarize" {
		t.Fatalf("journal replaced compaction input: %+v", summary.Messages)
	}
	if err := journal.After(resumed, &hooks.Event{Type: hooks.EventModelCallAfter, Input: summary, Output: &model.ChatResponse{Content: "partial summary"}}); err != nil {
		t.Fatal(err)
	}
	round, err := journal.load(resumed)
	if err != nil || round.Recovered || len(round.Calls) != 1 || round.Calls[0].State != "planned" {
		t.Fatalf("compaction cleared pending recovery: %+v error=%v", round, err)
	}
	live := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "continue"}, {Role: model.RoleUser, Name: workingContextName, TurnScoped: true, Content: "current task state"}}}
	if err := journal.Before(resumed, &hooks.Event{Type: hooks.EventModelCallBefore, Input: live}); err != nil {
		t.Fatal(err)
	}
	if live.Messages[len(live.Messages)-1].Content != "current task state" {
		t.Fatal("recovery lost live context tail")
	}
	for _, msg := range live.Messages {
		if msg.Content == "old task state" {
			t.Fatal("recovery restored stale transient state")
		}
	}
}

func TestInteractiveJournalHostCrashPreventsDuplicateEffect(t *testing.T) {
	const childEnv = "CHRONOS_JOURNAL_CRASH_FIXTURE"
	if root := os.Getenv(childEnv); root != "" {
		store, err := sqlite.New(filepath.Join(root, "sessions.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{{ID: "crash-effect", Name: "fixture_effect", Arguments: `{"target":"effect.txt"}`}}}
		a := newExecutionTestAgent("crash-fixture", provider)
		a.Storage = store
		a.Hooks = append(a.Hooks, &interactiveJournal{a: a, root: root})
		a.Tools.Register(&tool.Definition{Name: "fixture_effect", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
			file, err := os.OpenFile(filepath.Join(root, "effect.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			if _, err := file.WriteString("effect\n"); err != nil {
				file.Close()
				return nil, err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
			fmt.Println("EFFECT_WRITTEN")
			for {
				time.Sleep(time.Hour)
			}
		}})
		ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "crash-session"), agent.RunIdentity{InvocationID: "before-crash"})
		if _, err := a.Chat(ctx, "perform one fixture effect"); err != nil {
			t.Fatal(err)
		}
		t.Fatal("fixture unexpectedly returned")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInteractiveJournalHostCrashPreventsDuplicateEffect$")
	cmd.Env = append(os.Environ(), childEnv+"="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	observed := false
	for scanner.Scan() {
		if scanner.Text() == "EFFECT_WRITTEN" {
			observed = true
			break
		}
	}
	if !observed {
		t.Fatalf("child did not reach crash window: %v", scanner.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited successfully despite forced host termination")
	}
	store, err := sqlite.New(filepath.Join(root, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := newExecutionTestAgent("crash-fixture", &guardTestProvider{id: "gpt-4o"})
	a.Storage = store
	journal := &interactiveJournal{a: a, root: root}
	resumed := agent.WithRunIdentity(storage.WithSession(context.Background(), "crash-session"), agent.RunIdentity{InvocationID: "after-crash"})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "continue"}}}
	if err := journal.Before(resumed, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range req.Messages {
		if msg.ToolCallID == "crash-effect" && strings.Contains(strings.ToLower(msg.Content), "unknown") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing interrupted-effect evidence: %+v", req.Messages)
	}
	if err := journal.Before(resumed, &hooks.Event{Type: hooks.EventToolCallBefore, Name: "fixture_effect", Input: map[string]any{"target": "effect.txt"}}); err == nil {
		t.Fatal("unresolved effect could be repeated")
	}
	data, err := os.ReadFile(filepath.Join(root, "effect.txt"))
	if err != nil || string(data) != "effect\n" {
		t.Fatalf("effect repeated or lost: %q error=%v", data, err)
	}
}
