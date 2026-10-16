package orchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

func scopedRulesContext(root, invocation string) context.Context {
	return agent.WithRunIdentity(builtins.WithWorkspaceRoot(context.Background(), root), agent.RunIdentity{TenantID: "tenant", RepositoryID: "repo", SessionID: "session", RoleID: "coder", InvocationID: invocation})
}
func scopedRulesFile(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func scopedRulesTool(h *scopedRulesHook, ctx context.Context, name, path string) error {
	return h.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Name: name, Input: map[string]any{"path": path}})
}
func scopedRulesPin(t *testing.T, h *scopedRulesHook, ctx context.Context) string {
	t.Helper()
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "task"}}}
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if err := h.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: &model.ChatResponse{Role: model.RoleAssistant, Content: "rules observed"}}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, m := range req.Messages {
		if m.Name == "harness-scoped-rules" {
			text.WriteString(m.Content)
		}
	}
	return text.String()
}
func TestScopedRulesFirstCreateNeedsModelInstructions(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", "PARENT_RULE")
	scopedRulesFile(t, root, "src/new/Claude.md", "LEAF_RULE")
	scopedRulesFile(t, root, ".cursor/rules/code.mdc", "---\nglobs: src/**/*.go\nalwaysApply: false\ndescription: Go policy\n---\nMDC_RULE")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "first")
	err := scopedRulesTool(h, ctx, "file_write", "src/new/deeper/new.go")
	if !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("first create = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src/new/deeper/new.go")); !os.IsNotExist(err) {
		t.Fatal("preflight created file")
	}
	pin := scopedRulesPin(t, h, ctx)
	for _, want := range []string{"PARENT_RULE", "LEAF_RULE", "MDC_RULE", "description: Go policy"} {
		if !strings.Contains(pin, want) {
			t.Fatalf("missing %q: %s", want, pin)
		}
	}
	if strings.Index(pin, "PARENT_RULE") > strings.Index(pin, "LEAF_RULE") {
		t.Fatal("precedence reversed")
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/new/deeper/new.go"); err != nil {
		t.Fatalf("retry = %v", err)
	}
}
func TestScopedRulesParallelFirstCallsBlocked(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", "RULE")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "parallel")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := scopedRulesTool(h, ctx, "file_edit", fmt.Sprintf("src/%d.go", i)); !errors.Is(err, errScopedInstructionsNeeded) {
				t.Errorf("call %d = %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	pin := scopedRulesPin(t, h, ctx)
	if strings.Count(pin, "\nRULE\n") != 1 {
		t.Fatalf("rule not deduplicated: %s", pin)
	}
	if err := scopedRulesTool(h, ctx, "file_edit", "src/0.go"); err != nil {
		t.Fatal(err)
	}
}
func TestScopedRulesFullCriticalTailsAndMoreThanEightPaths(t *testing.T) {
	root := t.TempDir()
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "many")
	for i := 0; i < 12; i++ {
		dir := fmt.Sprintf("scope%d", i)
		scopedRulesFile(t, root, dir+"/AGENTS.md", strings.Repeat("policy ", 800)+fmt.Sprintf("CRITICAL_TAIL_%d", i))
		if err := scopedRulesTool(h, ctx, "file_read", dir+"/source.go"); !errors.Is(err, errScopedInstructionsNeeded) {
			t.Fatal(err)
		}
	}
	pin := scopedRulesPin(t, h, ctx)
	for i := 0; i < 12; i++ {
		if !strings.Contains(pin, fmt.Sprintf("CRITICAL_TAIL_%d", i)) {
			t.Fatalf("lost tail/scope %d", i)
		}
		if err := scopedRulesTool(h, ctx, "file_read", fmt.Sprintf("scope%d/source.go", i)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestScopedRulesRuleChangeRefreshAndSummaryDoesNotDeliver(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", "OLD")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "change")
	_ = scopedRulesTool(h, ctx, "file_read", "src/main.go")
	scopedRulesPin(t, h, ctx)
	scopedRulesFile(t, root, "src/AGENTS.md", "NEW")
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("changed = %v", err)
	}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a conversation summarizer. Summarize."}}}
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("summary delivered = %v", err)
	}
	if pin := scopedRulesPin(t, h, ctx); !strings.Contains(pin, "NEW") || strings.Contains(pin, "\nOLD\n") {
		t.Fatal(pin)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); err != nil {
		t.Fatal(err)
	}
}
func TestScopedRulesOwnershipIsolation(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", "PRIVATE_RULE")
	h := &scopedRulesHook{root: root}
	a := scopedRulesContext(root, "a")
	b := scopedRulesContext(root, "b")
	_ = scopedRulesTool(h, a, "file_read", "src/a.go")
	scopedRulesPin(t, h, a)
	if pin := scopedRulesPin(t, h, b); strings.Contains(pin, "PRIVATE_RULE") {
		t.Fatal("scope leaked")
	}
	if err := scopedRulesTool(h, b, "file_read", "src/a.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("delivery leaked: %v", err)
	}
	other := t.TempDir()
	scopedRulesFile(t, other, "src/AGENTS.md", "OTHER_RULE")
	if err := scopedRulesTool(h, scopedRulesContext(other, "a"), "file_read", "src/a.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("workspace leaked: %v", err)
	}
	differentTenant := agent.WithRunIdentity(a, agent.RunIdentity{TenantID: "other", RepositoryID: "repo", SessionID: "session", RoleID: "coder", InvocationID: "a"})
	if err := scopedRulesTool(h, differentTenant, "file_read", "src/a.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("tenant leaked: %v", err)
	}
	if err := scopedRulesTool(h, context.Background(), "file_read", "src/a.go"); err == nil {
		t.Fatal("anonymous scope allowed")
	}
}
func TestScopedRulesIgnoredSymlinkAndOutsideBounds(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "bounds")
	scopedRulesFile(t, root, ".gitignore", "generated/\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.go", filepath.Join(outside, "x.go"), "link/x.go", "link/../x.go", "dangling/x.go"} {
		if err := scopedRulesTool(h, ctx, "file_read", path); err == nil {
			t.Errorf("unsafe path %q allowed", path)
		}
	}
	for _, path := range []string{"node_modules/a.go", "generated/x.go"} {
		if err := scopedRulesTool(h, ctx, "file_read", path); err != nil {
			t.Fatalf("explicit path incorrectly denied by ignore rules: %v", err)
		}
	}
	scopedRulesFile(t, root, "src/file.go", "source")
	if err := os.Symlink(filepath.Join(outside, "rules"), filepath.Join(root, "src/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/file.go"); err == nil {
		t.Fatal("symlink instruction ignored")
	}
}

type scopedRulesRecoveryProvider struct {
	guardTestProvider
	t     *testing.T
	root  string
	calls int
}

func (p *scopedRulesRecoveryProvider) Name() string { return "scoped-rules-fixture" }

func (p *scopedRulesRecoveryProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	if p.calls == 2 {
		if _, err := os.Stat(filepath.Join(p.root, "src/new.go")); !os.IsNotExist(err) {
			p.t.Fatal("first tool call had an effect")
		}
		var instructions, recoverable bool
		for _, message := range req.Messages {
			instructions = instructions || (message.Name == "harness-scoped-rules" && strings.Contains(message.Content, "CREATE_RULE"))
			recoverable = recoverable || (message.Role == model.RoleTool && strings.Contains(message.Content, "scoped_instructions_needed"))
		}
		if !instructions || !recoverable {
			p.t.Fatalf("missing instructions/recoverable tool result: %+v", req.Messages)
		}
	}
	if p.calls <= 2 {
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("create-%d", p.calls), Name: "file_write", Arguments: `{"path":"src/new.go","content":"created"}`}}}, nil
	}
	return &model.ChatResponse{Content: "done", StopReason: model.StopReasonEnd}, nil
}

func TestScopedRulesRuntimeRecoverableFirstCreate(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", "CREATE_RULE")
	provider := &scopedRulesRecoveryProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, t: t, root: root}
	a := newExecutionTestAgent("scoped", provider)
	a.Storage = pipelineAgent(t).Storage
	write := builtins.NewFileWriteTool(root)
	write.Permission = tool.PermAllow
	a.Tools.Register(write)
	a.Hooks = append(a.Hooks, &scopedRulesHook{root: root})
	if _, err := a.Chat(scopedRulesContext(root, "runtime"), "create a file"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src/new.go"))
	if err != nil || string(data) != "created" || provider.calls != 3 {
		t.Fatalf("retry did not complete: content=%q calls=%d error=%v", data, provider.calls, err)
	}
}

func TestScopedRulesRootStartupAndMDCChanges(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "AGENTS.md", "ROOT_PINNED")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &scopedRulesHook{root: root, startupHashes: map[string][sha256.Size]byte{filepath.Join(canonicalRoot, "AGENTS.md"): sha256.Sum256([]byte("ROOT_PINNED"))}}
	ctx := scopedRulesContext(root, "root")
	if err := scopedRulesTool(h, ctx, "file_read", "main.go"); err != nil {
		t.Fatalf("startup rules unnecessarily blocked: %v", err)
	}
	scopedRulesFile(t, root, "AGENTS.md", "ROOT_CHANGED")
	if err := scopedRulesTool(h, ctx, "file_read", "main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("root change = %v", err)
	}
	scopedRulesPin(t, h, ctx)
	scopedRulesFile(t, root, ".cursor/rules/go.mdc", "---\nglobs: ['*.go']\nalwaysApply: false\n---\nMDC_OLD")
	if err := scopedRulesTool(h, ctx, "file_read", "main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("new MDC = %v", err)
	}
	scopedRulesPin(t, h, ctx)
	scopedRulesFile(t, root, ".cursor/rules/go.mdc", "---\nglobs: ['*.go']\nalwaysApply: false\n---\nMDC_NEW")
	if err := scopedRulesTool(h, ctx, "file_read", "main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("changed MDC = %v", err)
	}
	if pin := scopedRulesPin(t, h, ctx); !strings.Contains(pin, "MDC_NEW") {
		t.Fatal(pin)
	}
}

func TestScopedRulesStartupSnapshotCannotAcceptChangedRoot(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &scopedRulesHook{root: root, startupHashes: map[string][sha256.Size]byte{filepath.Join(canonicalRoot, "AGENTS.md"): sha256.Sum256([]byte("STARTUP"))}}
	scopedRulesFile(t, root, "AGENTS.md", "CHANGED_BEFORE_FIRST_EVENT")
	ctx := scopedRulesContext(root, "startup")
	if err := scopedRulesTool(h, ctx, "file_read", "source.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("changed startup = %v", err)
	}
	if pin := scopedRulesPin(t, h, ctx); !strings.Contains(pin, "CHANGED_BEFORE_FIRST_EVENT") {
		t.Fatal(pin)
	}
}

func TestScopedRulesMDCFullTailAndMetadataMatching(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, ".cursor/rules/large.mdc", "---\nglobs: src/{one,two}/**/*.go, other/**/*.go\nalwaysApply: false\ndescription: Keep the entire policy\n---\n"+strings.Repeat("policy ", 12000)+"CRITICAL_MDC_TAIL")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "mdc")
	if err := scopedRulesTool(h, ctx, "file_write", "unrelated/a.go"); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/two/new.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("brace metadata match = %v", err)
	}
	if pin := scopedRulesPin(t, h, ctx); !strings.Contains(pin, "CRITICAL_MDC_TAIL") || !strings.Contains(pin, "description: Keep the entire policy") {
		t.Fatal("MDC tail/metadata lost")
	}
}

func TestScopedRulesIgnoredRuleDirectoriesAndScanLimit(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, ".gitignore", ".cursor/rules/generated/\n")
	scopedRulesFile(t, root, ".cursor/rules/generated/invalid.mdc", "not frontmatter")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "scan")
	if err := scopedRulesTool(h, ctx, "file_read", "source.go"); err != nil {
		t.Fatalf("ignored rules traversed: %v", err)
	}
	for i := 0; i < maxScopedRulesEntries; i++ {
		scopedRulesFile(t, root, fmt.Sprintf(".cursor/rules/%d.txt", i), "not a rule")
	}
	if err := scopedRulesTool(h, ctx, "file_read", "source.go"); err == nil || !strings.Contains(err.Error(), "scan entry limit") {
		t.Fatalf("scan silently incomplete: %v", err)
	}
}

func TestScopedRulesStateLimitAndExplicitRelease(t *testing.T) {
	root := t.TempDir()
	h := &scopedRulesHook{root: root}
	for i := 0; i < maxScopedRulesStates; i++ {
		if err := scopedRulesTool(h, scopedRulesContext(root, fmt.Sprint(i)), "file_read", "a.go"); err != nil {
			t.Fatal(err)
		}
	}
	if err := scopedRulesTool(h, scopedRulesContext(root, "overflow"), "file_read", "a.go"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("state overflow = %v", err)
	}
	if err := scopedRulesTool(h, scopedRulesContext(root, "0"), "file_read", "a.go"); err != nil {
		t.Fatalf("state evicted: %v", err)
	}
	h.release(scopedRulesContext(root, "0"))
	if err := scopedRulesTool(h, scopedRulesContext(root, "overflow"), "file_read", "a.go"); err != nil {
		t.Fatal(err)
	}
}

func TestScopedRulesExplicitLimitsDoNotEvict(t *testing.T) {
	root := t.TempDir()
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "limits")
	for i := 0; i < maxScopedRulesPaths; i++ {
		if err := scopedRulesTool(h, ctx, "file_read", fmt.Sprintf("scope%d/a.go", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := scopedRulesTool(h, ctx, "file_read", "overflow/a.go"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("overflow = %v", err)
	}
	if err := scopedRulesTool(h, ctx, "file_read", "scope0/a.go"); err != nil {
		t.Fatalf("old scope evicted: %v", err)
	}
	scopedRulesFile(t, root, "scope0/AGENTS.md", strings.Repeat("x", maxScopedRulesFileBytes+1))
	if err := scopedRulesTool(h, ctx, "file_read", "scope0/a.go"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("large rule silently omitted: %v", err)
	}
}

func TestScopedRulesOnlySuccessfulIntactModelDeliveryAllowsAccess(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/AGENTS.md", strings.Repeat("rule ", 1400)+"CRITICAL_TAIL")
	h := &scopedRulesHook{root: root}
	ctx := scopedRulesContext(root, "delivery")
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatal(err)
	}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "implement the change"}}}
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("preflight counted as delivery: %v", err)
	}
	evt := &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: &model.ChatResponse{Content: "observed"}, Error: errors.New("provider unavailable")}
	if err := h.After(ctx, evt); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("failed call counted as delivery: %v", err)
	}
	evt.Error = nil
	full := req.Messages[len(req.Messages)-1].Content
	req.Messages[len(req.Messages)-1].Content = "truncated"
	if err := h.After(ctx, evt); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); !errors.Is(err, errScopedInstructionsNeeded) {
		t.Fatalf("partial rules counted as delivery: %v", err)
	}
	req.Messages[len(req.Messages)-1].Content = full
	if err := h.After(ctx, evt); err != nil {
		t.Fatal(err)
	}
	if err := scopedRulesTool(h, ctx, "file_write", "src/main.go"); err != nil {
		t.Fatalf("intact delivery not accepted: %v", err)
	}
	releaseScopedRuleState(ctx, &agent.Agent{Hooks: hooks.Chain{h}})
	h.mu.Lock()
	states := len(h.states)
	h.mu.Unlock()
	if states != 0 {
		t.Fatal("invocation state retained after completion")
	}
}
