package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/claims"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/verification"
)

const claimsSource = "package p\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\n\treturn 2\n}\n"

// claimsHarness wires a ranged file_read, a file_write and a mutating shell
// through the real verification observer.
func claimsHarness(t *testing.T) (*taskRuntime, *tool.Registry, context.Context, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(claimsSource), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := newTaskRuntime("task", root)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	registry.Register(&tool.Definition{Name: "file_read", Handler: func(_ context.Context, args map[string]any) (any, error) {
		return map[string]any{
			"path":       filepath.Join(root, args["path"].(string)),
			"start_line": args["start_line"],
			"end_line":   args["end_line"],
		}, nil
	}})
	registry.Register(&tool.Definition{Name: "file_write", Handler: func(_ context.Context, args map[string]any) (any, error) {
		return nil, os.WriteFile(filepath.Join(root, args["path"].(string)), []byte(args["content"].(string)), 0o600)
	}})
	registry.Register(&tool.Definition{Name: "shell", Handler: func(_ context.Context, args map[string]any) (any, error) {
		return map[string]any{"exit_code": 0}, os.WriteFile(filepath.Join(root, "p.go"), []byte(args["content"].(string)), 0o600)
	}})
	wrapVerificationEvidence(&agent.Agent{ID: "coder", Tools: registry})
	return runtime, registry, withTaskRuntime(context.Background(), runtime), root
}

func readRange(t *testing.T, registry *tool.Registry, ctx context.Context, start, end int) {
	t.Helper()
	if _, err := registry.Execute(ctx, "file_read", map[string]any{"path": "p.go", "start_line": start, "end_line": end}); err != nil {
		t.Fatal(err)
	}
}

func claimEvents(t *testing.T, runtime *taskRuntime) []execution.Event {
	t.Helper()
	events := runtime.ledger.Events()
	var out []execution.Event
	for _, event := range events {
		if event.Type == execution.EventClaim {
			out = append(out, event)
		}
	}
	return out
}

func TestTaskClaimsWriteToReadSpanMarksClaimStale(t *testing.T) {
	runtime, registry, ctx, _ := claimsHarness(t)
	readRange(t, registry, ctx, 3, 5) // func A
	readRange(t, registry, ctx, 7, 9) // func B
	readRange(t, registry, ctx, 3, 5) // duplicate read is not a new claim
	if got := runtime.claims.List(); len(got) != 2 {
		t.Fatalf("claims = %#v, want 2 deduplicated read claims", got)
	}

	edited := strings.Replace(claimsSource, "return 1", "return 42", 1)
	if _, err := registry.Execute(ctx, "file_write", map[string]any{"path": "p.go", "content": edited}); err != nil {
		t.Fatal(err)
	}
	list := runtime.claims.List()
	if list[0].Status != claims.StatusStale || list[1].Status != claims.StatusLive {
		t.Fatalf("statuses = %s, %s; want stale, live", list[0].Status, list[1].Status)
	}
	events := claimEvents(t, runtime)
	if len(events) != 1 || !strings.Contains(events[0].Detail, "live -> stale") || events[0].Paths[0] != "p.go" {
		t.Fatalf("claim events = %#v", events)
	}
	// Claim transitions are audit-only: the write, not the claim event,
	// is what invalidates verification.
	state, err := runtime.snapshot()
	if err != nil || len(state.Writes) != 1 {
		t.Fatalf("writes = %#v, %v", state.Writes, err)
	}

	prompt := buildRepairPrompt(verification.Decision{}, runtime)
	staleAt := strings.Index(prompt, "[stale] c1 read p.go:3-5")
	liveAt := strings.Index(prompt, "[live] c2 read p.go:7-9")
	if staleAt < 0 || liveAt < 0 || staleAt > liveAt || !strings.Contains(prompt, "re-read before relying") {
		t.Fatalf("repair prompt lacks ordered working memory:\n%s", prompt)
	}
}

func TestTaskClaimsMovedSpanStaysLive(t *testing.T) {
	runtime, registry, ctx, _ := claimsHarness(t)
	readRange(t, registry, ctx, 7, 9)
	moved := strings.Replace(claimsSource, "package p\n", "package p\n\n// inserted\n// lines\n", 1)
	if _, err := registry.Execute(ctx, "file_write", map[string]any{"path": "p.go", "content": moved}); err != nil {
		t.Fatal(err)
	}
	claim := runtime.claims.List()[0]
	if claim.Status != claims.StatusLive || claim.Anchors[0].StartLine != 10 || claim.Anchors[0].EndLine != 12 {
		t.Fatalf("moved claim = %#v, want live at 10-12", claim)
	}
	if events := claimEvents(t, runtime); len(events) != 0 {
		t.Fatalf("relocation must not emit a transition: %#v", events)
	}
}

func TestTaskClaimsShellMutationDoubtsDerivedClaims(t *testing.T) {
	runtime, registry, ctx, _ := claimsHarness(t)
	readRange(t, registry, ctx, 3, 5)
	child, err := runtime.claims.Add(claims.Input{
		Text:        "B mirrors A",
		Anchors:     []claims.Anchor{{Path: "p.go", StartLine: 7, EndLine: 9}},
		DerivedFrom: []string{"c1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(claimsSource, "return 1", "return 7", 1)
	if _, err := registry.Execute(ctx, "shell", map[string]any{"command": "sed -i s/1/7/ p.go", "content": edited}); err != nil {
		t.Fatal(err)
	}
	got, _ := runtime.claims.Get(child.ID)
	if got.Status != claims.StatusDoubted {
		t.Fatalf("derived claim = %#v, want doubted", got)
	}
	if events := claimEvents(t, runtime); len(events) != 2 {
		t.Fatalf("claim events = %#v, want stale parent + doubted child", events)
	}
	if prompt := buildRepairPrompt(verification.Decision{}, runtime); !strings.Contains(prompt, "[doubted] c2 B mirrors A") {
		t.Fatalf("repair prompt lacks doubted claim:\n%s", prompt)
	}
}

func TestTaskClaimsSkipsUnanchorableReads(t *testing.T) {
	runtime, _, _, root := claimsHarness(t)
	path := filepath.Join(root, "p.go")
	runtime.recordReadClaim(map[string]any{"path": "p.go"}, map[string]any{"path": path, "content": claimsSource})
	runtime.recordReadClaim(map[string]any{"path": "p.go", "start_line": 1}, map[string]any{"path": path, "outline": true})
	runtime.recordReadClaim(map[string]any{"path": "../outside.go", "start_line": 1, "end_line": 2}, map[string]any{"compressed": true})
	if got := runtime.claims.List(); len(got) != 0 {
		t.Fatalf("claims = %#v, want none", got)
	}
	// A compressed result falls back to the requested (JSON-number) range,
	// clamped to the file.
	runtime.recordReadClaim(map[string]any{"path": "p.go", "start_line": float64(7), "end_line": float64(500)}, map[string]any{"compressed": true})
	got := runtime.claims.List()
	if len(got) != 1 || got[0].Anchors[0].StartLine != 7 || got[0].Anchors[0].EndLine != 10 {
		t.Fatalf("claims = %#v, want one clamped 7-10 claim", got)
	}
}

func TestTaskClaimsDigestIsBounded(t *testing.T) {
	runtime, registry, ctx, _ := claimsHarness(t)
	for start := 1; start <= maxPromptClaims+3; start++ {
		readRange(t, registry, ctx, 1, 1+start%10)
		readRange(t, registry, ctx, 1+start%9, 10)
	}
	digest := runtime.claimsDigest()
	if len(digest) != maxPromptClaims+1 || !strings.Contains(digest[0], "live claims omitted") {
		t.Fatalf("digest = %q", digest)
	}
}
