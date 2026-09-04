package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"
	storagesqlite "github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/toolcompress"
)

func TestSetupSessionsStartupSelection(t *testing.T) {
	ctx := context.Background()
	store, err := storagesqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New(:memory:) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	mgr := session.NewManager(store, "")
	now := time.Now()
	if err := store.CreateSession(ctx, &storage.Session{ID: "existing-session", AgentID: "coder", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateSession(existing session) error = %v", err)
	}
	agents := map[string]*agent.Agent{"coder": {ID: "coder"}}

	t.Run("ordinary startup creates fresh session", func(t *testing.T) {
		got := setupSessions(ctx, &config.Config{}, mgr, agents, "")["coder"]
		if got == "" || got == "existing-session" {
			t.Fatalf("session = %q, want a fresh session", got)
		}
	})

	t.Run("explicit resume wins", func(t *testing.T) {
		got := setupSessions(ctx, &config.Config{}, mgr, agents, "existing-session")["coder"]
		if got != "existing-session" {
			t.Fatalf("session = %q, want explicit existing-session", got)
		}
	})

	t.Run("auto resume remains opt in", func(t *testing.T) {
		latest, err := mgr.Latest(ctx, "coder")
		if err != nil || latest == nil {
			t.Fatalf("Latest() = %+v, %v", latest, err)
		}
		got := setupSessions(ctx, &config.Config{Session: config.SessionConfig{AutoResume: true}}, mgr, agents, "")["coder"]
		if got != latest.ID {
			t.Fatalf("session = %q, want latest %q", got, latest.ID)
		}
	})
}

type routingTestProvider struct {
	provider string
	model    string
}

type closeTrackingStorage struct {
	storage.Storage
	mu     sync.Mutex
	closes int
	err    error
}

type failingSummaryStorage struct {
	storage.Storage
}

func (s failingSummaryStorage) ListSessions(context.Context, string, int, int) ([]*storage.Session, error) {
	return nil, errors.New("history unavailable")
}

type closeTrackingLSP struct {
	mu     sync.Mutex
	closes int
}

func (m *closeTrackingLSP) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes++
	return nil
}

func (m *closeTrackingLSP) closeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes
}

func (s *closeTrackingStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return s.err
}

func (s *closeTrackingStorage) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (p *routingTestProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) Name() string  { return p.provider }
func (p *routingTestProvider) Model() string { return p.model }

type skillContextTestProvider struct {
	mu       sync.Mutex
	requests []*model.ChatRequest
}

type budgetTestProvider struct {
	mu       sync.Mutex
	calls    int
	modelID  string
	response *model.ChatResponse
	err      error
}

type subagentTestProvider struct {
	name     string
	modelID  string
	response string
	mu       sync.Mutex
	requests []*model.ChatRequest
}

type recoverableDenialProvider struct {
	calls    int
	requests []*model.ChatRequest
}

func (p *recoverableDenialProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	p.requests = append(p.requests, req)
	if p.calls == 1 {
		return &model.ChatResponse{
			StopReason: model.StopReasonToolCall,
			ToolCalls:  []model.ToolCall{{ID: "denied-1", Name: "file_read", Arguments: `{"path":".env"}`}},
		}, nil
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "recovered", StopReason: model.StopReasonEnd}, nil
}

func (*recoverableDenialProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("unexpected streaming call")
}

func (*recoverableDenialProvider) Name() string  { return "recoverable-denial" }
func (*recoverableDenialProvider) Model() string { return "test-model" }

func (p *subagentTestProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return &model.ChatResponse{Role: model.RoleAssistant, Content: p.response, StopReason: model.StopReasonEnd}, nil
}

func (p *subagentTestProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("unexpected streaming call")
}

func (p *subagentTestProvider) Name() string  { return p.name }
func (p *subagentTestProvider) Model() string { return p.modelID }

func (p *budgetTestProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.response, p.err
}

func (p *budgetTestProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("unexpected streaming call")
}

func (p *budgetTestProvider) Name() string  { return "test" }
func (p *budgetTestProvider) Model() string { return p.modelID }

func (p *budgetTestProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newBudgetTestOrchestrator(provider model.Provider) *Orchestrator {
	a := &agent.Agent{
		ID:         "coder",
		Model:      provider,
		Tools:      tool.NewRegistry(),
		Guardrails: guardrails.NewEngine(),
	}
	orch := &Orchestrator{
		agents:   map[string]*agent.Agent{"coder": a},
		active:   "coder",
		sessions: map[string]string{"coder": "session-1"},
		budget:   budget.NewTracker(0, 500),
	}
	a.Hooks = append(a.Hooks, budgetHook{tracker: orch.budget, orchestrator: orch, agentID: a.ID})
	return orch
}

func TestSetupSubAgentsExecutesConfiguredWorker(t *testing.T) {
	parentProvider := &subagentTestProvider{name: "parent-provider", modelID: "parent", response: "parent"}
	workerProvider := &subagentTestProvider{name: "worker-provider", modelID: "worker", response: "worker analysis"}
	reviewerProvider := &subagentTestProvider{name: "reviewer-provider", modelID: "reviewer", response: "review findings"}
	parent, err := agent.New("coder", "Coder").WithModel(parentProvider).Build()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := agent.New("researcher", "Researcher").WithModel(workerProvider).Build()
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := agent.New("reviewer", "Reviewer").WithModel(reviewerProvider).Build()
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]*agent.Agent{"coder": parent, "researcher": worker, "reviewer": reviewer}

	if err := setupSubAgents(agents); err != nil {
		t.Fatalf("setupSubAgents() error = %v", err)
	}
	definition, ok := parent.Tools.Get("spawn_subagent")
	if !ok {
		t.Fatal("primary agent has no spawn_subagent tool")
	}
	if !strings.Contains(definition.Description, "researcher") || !strings.Contains(definition.Description, "reviewer") {
		t.Errorf("spawn_subagent description = %q, want configured workers", definition.Description)
	}
	result, err := parent.Tools.Execute(context.Background(), "spawn_subagent", map[string]any{
		"agent": "researcher",
		"task":  "analyze the project",
	})
	if err != nil {
		t.Fatalf("spawn_subagent execution error = %v", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok || resultMap["agent"] != "researcher" || resultMap["result"] != "worker analysis" {
		t.Errorf("spawn_subagent result = %#v", result)
	}
	orch := &Orchestrator{agents: agents, active: "coder", sessions: map[string]string{"coder": "session-1"}}
	direct, err := orch.RunSubagent(context.Background(), map[string]any{
		"agent": "researcher",
		"task":  "inspect directly",
	})
	if err != nil || direct != "worker analysis" {
		t.Fatalf("RunSubagent() = %q, %v", direct, err)
	}
	workerProvider.mu.Lock()
	defer workerProvider.mu.Unlock()
	if len(workerProvider.requests) != 2 {
		t.Fatalf("worker requests = %d, want 2", len(workerProvider.requests))
	}
	last := workerProvider.requests[1].Messages[len(workerProvider.requests[1].Messages)-1]
	if last.Content != "inspect directly" {
		t.Errorf("worker task = %q, want inspect directly", last.Content)
	}

	reviewResult, err := parent.Tools.Execute(context.Background(), "spawn_subagent", map[string]any{
		"agent": "reviewer",
		"task":  "review the project",
	})
	if err != nil {
		t.Fatalf("second spawn_subagent execution error = %v", err)
	}
	if got := reviewResult.(map[string]any)["result"]; got != "review findings" {
		t.Errorf("reviewer result = %#v, want review findings", got)
	}
}

func TestConfiguredAgentRunnerRejectsDelegationCycle(t *testing.T) {
	runner := configuredAgentRunner{}
	ctx := context.WithValue(context.Background(), subagentPathKey{}, []string{"coder", "researcher"})
	_, err := runner.Run(ctx, harness.SubAgentSpec{Name: "researcher"}, "recurse")
	if err == nil || !strings.Contains(err.Error(), "delegation cycle") {
		t.Fatalf("cyclic Run() error = %v", err)
	}
}

func TestConfiguredAgentRunnerDoesNotCapIndependentDelegations(t *testing.T) {
	provider := &subagentTestProvider{name: "worker-provider", modelID: "worker", response: "done"}
	worker, err := agent.New("researcher", "Researcher").WithModel(provider).Build()
	if err != nil {
		t.Fatal(err)
	}
	runner := configuredAgentRunner{agents: map[string]*agent.Agent{"researcher": worker}}
	ctx := withSubagentTurnState(context.Background(), 0, "coder")
	spec := harness.SubAgentSpec{Name: "researcher"}
	for i := 0; i < 6; i++ {
		if _, err := runner.Run(ctx, spec, "bounded task"); err != nil {
			t.Fatalf("Run() %d error = %v", i+1, err)
		}
	}
}

func TestTurnModelCallLimit(t *testing.T) {
	ctx := withSubagentTurnState(context.Background(), 2, "coder")
	if err := claimTurnModelCall(ctx); err != nil {
		t.Fatalf("first model call rejected: %v", err)
	}
	if err := claimTurnModelCall(ctx); err != nil {
		t.Fatalf("second model call rejected: %v", err)
	}
	if err := claimTurnModelCall(ctx); err == nil || !strings.Contains(err.Error(), "model call limit exceeded") {
		t.Fatalf("third model call error = %v", err)
	}
}

func TestBudgetRejectsBeforeProviderInvocation(t *testing.T) {
	provider := &budgetTestProvider{modelID: "claude-haiku-4-5"}
	orch := newBudgetTestOrchestrator(provider)
	orch.SetUSDCap(1)

	_, err := orch.Chat(context.Background(), "hello")
	if !errors.Is(err, budget.ErrUSDBudgetExceeded) {
		t.Fatalf("Chat() error = %v, want ErrUSDBudgetExceeded", err)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	if got := orch.SessionCost(); got != (budget.SessionCost{}) {
		t.Fatalf("SessionCost() = %+v, want no spend or reservation", got)
	}
}

func TestCompactActiveSession_NoActiveAgent(t *testing.T) {
	orch := &Orchestrator{agents: map[string]*agent.Agent{}, active: "missing"}
	if err := orch.CompactActiveSession(context.Background()); err == nil {
		t.Fatal("expected error for missing active agent")
	}
}

func TestCompactActiveSession_PropagatesAgentError(t *testing.T) {
	// newBudgetTestOrchestrator's agent has no Storage, so the underlying
	// agent.CompactSession call itself errors; CompactActiveSession must
	// surface that rather than silently succeeding.
	orch := newBudgetTestOrchestrator(&budgetTestProvider{modelID: "claude-haiku-4-5"})
	err := orch.CompactActiveSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "compact session") {
		t.Fatalf("CompactActiveSession() error = %v, want a wrapped compact session error", err)
	}
}

func TestCompactActiveSession_ResetsBudgetOnSuccess(t *testing.T) {
	store := storagememory.New()
	provider := &subagentTestProvider{name: "p", modelID: "test-model", response: "ok"}
	a, err := agent.New("coder", "Coder").WithModel(provider).WithStorage(store).Build()
	if err != nil {
		t.Fatalf("agent.New().Build() error = %v", err)
	}

	tracker := budget.NewTracker(1000, 500)
	orch := &Orchestrator{
		agents:   map[string]*agent.Agent{"coder": a},
		active:   "coder",
		sessions: map[string]string{"coder": "session-1"},
		budget:   tracker,
	}

	// Simulate prior usage accumulated against this session, so resetting
	// it afterward is actually observable.
	if err := tracker.After(context.Background(), &hooks.Event{
		Type:   hooks.EventModelCallAfter,
		Name:   "session-1",
		Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 100}},
	}); err != nil {
		t.Fatalf("seed tracker usage: %v", err)
	}
	if got := tracker.Used("session-1"); got != 100 {
		t.Fatalf("Used(session-1) before compaction = %d, want 100", got)
	}

	// The session has no persisted events, so agent.CompactSession is a
	// no-op success (nothing to summarize) — this test is about
	// CompactActiveSession's own budget-reset wiring, not the SDK's
	// summarization logic (covered separately in the chronos repo).
	if err := orch.CompactActiveSession(context.Background()); err != nil {
		t.Fatalf("CompactActiveSession() error = %v", err)
	}
	if got := tracker.Used("session-1"); got != 0 {
		t.Fatalf("Used(session-1) after compaction = %d, want 0", got)
	}
}

func TestLearningTelemetryEnabledCreatesStoreAndIsolatesAgents(t *testing.T) {
	root := t.TempDir()
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder"},
		"reviewer": {ID: "reviewer"},
	}
	store, err := setupLearningTelemetry(context.Background(), &config.Config{
		Learning: config.LearningConfig{Enabled: true},
	}, root, agents)
	if err != nil {
		t.Fatalf("setupLearningTelemetry() error = %v", err)
	}
	if store == nil {
		t.Fatal("setupLearningTelemetry() store = nil, want SQL store")
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	dbPath := filepath.Join(root, config.ConfigDirName, "memory.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("Stat(%q) error = %v", dbPath, err)
	}
	if len(agents["coder"].Hooks) != 1 || len(agents["reviewer"].Hooks) != 1 {
		t.Fatalf("agent hooks = (%#v, %#v), want one telemetry recorder each", agents["coder"].Hooks, agents["reviewer"].Hooks)
	}
	coderRecorder, coderOK := agents["coder"].Hooks[0].(*learning.TelemetryRecorder)
	reviewerRecorder, reviewerOK := agents["reviewer"].Hooks[0].(*learning.TelemetryRecorder)
	if !coderOK || !reviewerOK {
		t.Fatalf("agent hooks = (%#v, %#v), want telemetry recorders", agents["coder"].Hooks, agents["reviewer"].Hooks)
	}
	if coderRecorder == reviewerRecorder {
		t.Fatal("agents share one telemetry recorder, want isolated recorders")
	}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "test-model"}
	if err := coderRecorder.Before(context.Background(), evt); err != nil {
		t.Fatalf("coder recorder Before() error = %v", err)
	}
	if err := reviewerRecorder.Before(context.Background(), evt); err != nil {
		t.Fatalf("reviewer recorder Before() error = %v", err)
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Sessions != 2 || stats.Turns != 2 {
		t.Fatalf("Stats() = %+v, want two isolated agent sessions and turns", stats)
	}
}

func TestLearningTelemetryDisabledCreatesNothing(t *testing.T) {
	root := t.TempDir()
	a := &agent.Agent{ID: "coder"}
	store, err := setupLearningTelemetry(context.Background(), &config.Config{}, root, map[string]*agent.Agent{"coder": a})
	if err != nil {
		t.Fatalf("setupLearningTelemetry() error = %v", err)
	}
	if store != nil {
		t.Fatal("setupLearningTelemetry() store is non-nil when learning is disabled")
	}
	if len(a.Hooks) != 0 {
		t.Fatalf("agent hooks = %#v, want no telemetry recorder", a.Hooks)
	}
	if _, err := os.Stat(filepath.Join(root, config.ConfigDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled learning created config directory: %v", err)
	}
}

func TestLearningTelemetryInitializationFailureDoesNotAttachRecorders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	a := &agent.Agent{ID: "coder"}
	store, err := setupLearningTelemetry(context.Background(), &config.Config{
		Learning: config.LearningConfig{Enabled: true},
	}, root, map[string]*agent.Agent{"coder": a})
	if err == nil || !strings.Contains(err.Error(), "create learning telemetry directory") {
		t.Fatalf("setupLearningTelemetry() error = %v, want contextual directory error", err)
	}
	if store != nil || len(a.Hooks) != 0 {
		t.Fatalf("failed setup returned store %v and hooks %#v, want neither", store, a.Hooks)
	}
}

func TestLearningTelemetryCloseIsOnceAndComposesErrors(t *testing.T) {
	root := t.TempDir()
	learningStore, err := setupLearningTelemetry(context.Background(), &config.Config{
		Learning: config.LearningConfig{Enabled: true},
	}, root, nil)
	if err != nil {
		t.Fatalf("setupLearningTelemetry() error = %v", err)
	}
	storageErr := errors.New("primary storage close failed")
	primary := &closeTrackingStorage{err: storageErr}
	orch := &Orchestrator{learningStore: learningStore, store: primary}

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- orch.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, storageErr) {
			t.Fatalf("Close() error = %v, want joined primary storage error", err)
		}
	}
	if got := primary.closeCount(); got != 1 {
		t.Fatalf("primary storage Close() calls = %d, want 1", got)
	}
	if _, err := learningStore.Stats(context.Background()); err == nil {
		t.Fatal("learning store remains usable after Orchestrator.Close()")
	}
}

func TestLSPCloseIsExactlyOnce(t *testing.T) {
	manager := &closeTrackingLSP{}
	orch := &Orchestrator{lspManager: manager}

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = orch.Close()
		}()
	}
	wg.Wait()
	if got := manager.closeCount(); got != 1 {
		t.Fatalf("LSP manager Close() calls = %d, want 1", got)
	}
}

func TestBudgetReconcilesSuccessfulCallOnce(t *testing.T) {
	provider := &budgetTestProvider{
		modelID: "claude-haiku-4-5",
		response: &model.ChatResponse{
			Role:  model.RoleAssistant,
			Usage: model.Usage{PromptTokens: 4, CompletionTokens: 2},
		},
	}
	orch := newBudgetTestOrchestrator(provider)
	orch.SetUSDCap(1_000)

	if _, err := orch.Chat(context.Background(), "hello"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	want := budget.SessionCost{InputTokens: 4, OutputTokens: 2, SpentMicrodollars: 14}
	if got := orch.SessionCost(); got != want {
		t.Fatalf("SessionCost() = %+v, want %+v", got, want)
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestBudgetFailedCallReleasesReservation(t *testing.T) {
	provider := &budgetTestProvider{modelID: "claude-haiku-4-5", err: errors.New("provider failed")}
	orch := newBudgetTestOrchestrator(provider)
	orch.SetUSDCap(1_000)

	if _, err := orch.Chat(context.Background(), "hello"); err == nil {
		t.Fatal("Chat() error = nil, want provider failure")
	}
	if got := orch.SessionCost(); got != (budget.SessionCost{}) {
		t.Fatalf("SessionCost() = %+v, want released reservation and no spend", got)
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 for a non-retryable provider error", got)
	}
}

func TestBudgetUnknownModelRequiresPriceOnlyWithUSDCap(t *testing.T) {
	request := &model.ChatRequest{Model: "claude-sonnet-5", Messages: []model.Message{{Role: model.RoleUser, Content: "hello"}}}
	for _, tt := range []struct {
		name    string
		cap     budget.Microdollars
		wantErr bool
	}{
		{name: "unlimited", cap: 0},
		{name: "capped", cap: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orch := &Orchestrator{usdBudget: budget.NewTrackerWithUSDCap(0, 0, tt.cap)}
			hook := budgetHook{tracker: budget.NewTracker(0, 0), orchestrator: orch, agentID: "coder"}
			err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: request})
			if tt.wantErr && !errors.Is(err, budget.ErrUnknownModel) {
				t.Fatalf("Before() error = %v, want ErrUnknownModel", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Before() error = %v, want unknown unpriced model allowed without a USD cap", err)
			}
		})
	}
}

func (p *skillContextTestProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.record(req)
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "ok"}, nil
}

func (p *skillContextTestProvider) StreamChat(_ context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	p.record(req)
	ch := make(chan *model.ChatResponse, 1)
	ch <- &model.ChatResponse{Role: model.RoleAssistant, Content: "ok"}
	close(ch)
	return ch, nil
}

func (p *skillContextTestProvider) Name() string  { return "test" }
func (p *skillContextTestProvider) Model() string { return "test" }

func (p *skillContextTestProvider) record(req *model.ChatRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
}

func (p *skillContextTestProvider) request(index int) *model.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[index]
}

func writeTestSkill(t *testing.T, root, name, trigger, body string) {
	t.Helper()
	dir := filepath.Join(root, config.ConfigDirName, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := "---\nname: " + name + "\ndescription: test skill\ntriggers: [" + trigger + "]\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func newRoutingTestOrchestrator(t *testing.T, models map[router.Complexity]map[router.TaskKind]router.ModelSpec) *Orchestrator {
	t.Helper()
	cfg := &router.Config{
		IntentRouting: []router.IntentRoute{{Intent: "debug", Agent: "debugger", Patterns: []string{"fix"}}},
		ModelRouting:  router.ModelRouting{Models: models},
	}
	rt, err := router.New(cfg, "coder")
	if err != nil {
		t.Fatalf("router.New() error = %v", err)
	}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{
			"coder":    {ID: "coder", Model: &routingTestProvider{provider: "old", model: "old-coder"}},
			"debugger": {ID: "debugger", Model: &routingTestProvider{provider: "old", model: "old-debugger"}},
		},
		active:         "coder",
		router:         rt,
		routingConfig:  cfg,
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
	}
	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}
	return orch
}

func TestRouteAppliesResolvedModelWithoutChangingSelectedAgent(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityHigh: {
			router.TaskKindDebug: {Provider: "routed", Model: "high-debug"},
		},
		router.ComplexityMedium: {
			router.TaskKindEdit: {Provider: "fallback", Model: "medium-edit"},
		},
	})

	agentID, matched := orch.Route(context.Background(), "fix this bug across multiple files")
	if !matched || agentID != "debugger" {
		t.Fatalf("Route() = (%q, %v), want (debugger, true)", agentID, matched)
	}
	if orch.ActiveID() != "coder" {
		t.Fatalf("ActiveID() = %q, want Route to leave active agent unchanged", orch.ActiveID())
	}
	selected := orch.agents[agentID].Model
	if selected.Name() != "routed" || selected.Model() != "high-debug" {
		t.Fatalf("selected agent model = (%q, %q), want (routed, high-debug)", selected.Name(), selected.Model())
	}
}

func TestSelectPrimaryAgentPrefersChronosCode(t *testing.T) {
	got := selectPrimaryAgent(map[string]*agent.Agent{
		"coder":        {ID: "coder"},
		"chronos-code": {ID: "chronos-code"},
		"reviewer":     {ID: "reviewer"},
	}, []string{"coder", "chronos-code", "reviewer"})
	if got != DefaultPrimaryAgentID {
		t.Fatalf("selectPrimaryAgent() = %q, want %q", got, DefaultPrimaryAgentID)
	}
	got = selectPrimaryAgent(map[string]*agent.Agent{"reviewer": {ID: "reviewer"}}, []string{"reviewer"})
	if got != "reviewer" {
		t.Fatalf("selectPrimaryAgent() fallback = %q, want reviewer", got)
	}
}

func TestFormatRoutingHintStaysAdvisory(t *testing.T) {
	class := router.Classification{Complexity: router.ComplexityHigh, Kind: router.TaskKindDebug}
	path := router.DefaultPath(class.Complexity)
	hint := formatRoutingHint("chronos-code", "debug", "debugger", true, class, path, nil)
	if !strings.Contains(hint, "spawn_subagent debugger") || !strings.Contains(hint, "You remain chronos-code") || !strings.Contains(hint, "Path: complexity=high") {
		t.Fatalf("hint = %q", hint)
	}
	same := formatRoutingHint("debugger", "debug", "debugger", true, class, path, nil)
	if !strings.Contains(same, "Path:") || strings.Contains(same, "spawn_subagent debugger") {
		t.Fatalf("same-agent hint = %q", same)
	}
	shadow := &router.PPDDecision{Action: router.PPDActionShadow, Specialist: "ppd-planner"}
	low := router.Classification{Complexity: router.ComplexityLow, Kind: router.TaskKindEdit}
	if got := formatRoutingHint("chronos-code", "code", "chronos-code", false, low, router.DefaultPath(low.Complexity), shadow); !strings.Contains(got, "ppd-planner") || !strings.Contains(got, "Path:") {
		t.Fatalf("shadow hint = %q", got)
	}
	delegate := &router.PPDDecision{Action: router.PPDActionDelegate, Specialist: "ppd-planner"}
	if got := formatRoutingHint("ppd-planner", "code", "coder", true, class, path, delegate); got != "" {
		t.Fatalf("delegate hint = %q, want empty", got)
	}
}

func TestModelEscalationIsCapped(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
		router.ComplexityHigh:   {router.TaskKindEdit: {Provider: "routed", Model: "high"}},
	})
	builds := 0
	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		builds++
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}

	orch.Route(context.Background(), "change this")
	hook := modelEscalationHook{orchestrator: orch, agentID: "coder"}
	failedTool := &hooks.Event{Type: hooks.EventToolCallAfter, Error: errors.New("recoverable tool failure")}
	for range 3 {
		if err := hook.After(context.Background(), failedTool); err != nil {
			t.Fatalf("After() error = %v", err)
		}
	}
	if provider, modelID := orch.ActiveModelInfo(); provider != "routed" || modelID != "high" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want capped model (routed, high)", provider, modelID)
	}
	if builds != 3 {
		t.Fatalf("provider builds = %d, want 3 (route plus two escalations)", builds)
	}
}

func TestEscalationProviderFailurePreservesExistingModelAndLevel(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
	})
	orch.Route(context.Background(), "change this")
	original := orch.agents["coder"].Model
	orch.buildProvider = func(agent.ModelConfig) (model.Provider, error) {
		return nil, errors.New("provider construction failed")
	}
	hook := modelEscalationHook{orchestrator: orch, agentID: "coder"}
	failedTool := &hooks.Event{Type: hooks.EventToolCallAfter, Error: errors.New("recoverable tool failure")}

	if err := hook.After(context.Background(), failedTool); err != nil {
		t.Fatalf("After() error = %v", err)
	}
	if orch.agents["coder"].Model != original {
		t.Fatal("failed provider construction replaced the existing provider")
	}

	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}
	if err := hook.After(context.Background(), failedTool); err != nil {
		t.Fatalf("After() retry error = %v", err)
	}
	if provider, modelID := orch.ActiveModelInfo(); provider != "routed" || modelID != "medium" {
		t.Fatalf("ActiveModelInfo() after retry = (%q, %q), want unchanged escalation target (routed, medium)", provider, modelID)
	}
}

func TestSetThinking(t *testing.T) {
	a, err := agent.New("coder", "Coder").Build()
	if err != nil {
		t.Fatal(err)
	}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}
	if got := orch.ThinkingLevel(); got != "off" {
		t.Fatalf("ThinkingLevel() = %q, want off", got)
	}
	if err := orch.SetThinking("medium"); err != nil {
		t.Fatalf("SetThinking(medium) error = %v", err)
	}
	if got := orch.ThinkingLevel(); got != "medium" {
		t.Fatalf("ThinkingLevel() = %q, want medium", got)
	}
	if !a.ReasoningConfig.Enabled || a.ReasoningConfig.Effort != "medium" || a.ReasoningConfig.BudgetTokens != 4096 {
		t.Fatalf("ReasoningConfig = %#v", a.ReasoningConfig)
	}
	if err := orch.SetThinking("banana"); err == nil {
		t.Fatal("SetThinking(banana) succeeded")
	}
}

func TestRouteRespectsExplicitModelOverride(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
	})
	if err := orch.SwitchModel(context.Background(), "user", "chosen"); err != nil {
		t.Fatalf("SwitchModel() error = %v", err)
	}

	orch.Route(context.Background(), "change this")
	if provider, modelID := orch.ActiveModelInfo(); provider != "user" || modelID != "chosen" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want explicit override (user, chosen)", provider, modelID)
	}
}

func TestSkillContextParityPreservesExistingPins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeTestSkill(t, root, "parity-skill", "paritytoken", "parity skill body")

	provider := &skillContextTestProvider{}
	a := &agent.Agent{
		ID:         "coder",
		Model:      provider,
		Tools:      tool.NewRegistry(),
		Guardrails: guardrails.NewEngine(),
		ContextPinsFn: func(context.Context) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
		},
	}
	setupSkills(&config.Config{}, root, map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{
		agents:   map[string]*agent.Agent{"coder": a},
		active:   "coder",
		sessions: map[string]string{"coder": "session-1"},
	}

	if _, err := orch.Chat(context.Background(), "paritytoken"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	stream, err := orch.ChatStream(context.Background(), "paritytoken")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}

	blockingPins := systemContents(provider.request(0))
	streamingPins := systemContents(provider.request(1))
	if strings.Join(blockingPins, "\n") != strings.Join(streamingPins, "\n") {
		t.Fatalf("blocking pins %q differ from streaming pins %q", blockingPins, streamingPins)
	}
	joined := strings.Join(blockingPins, "\n")
	if !strings.Contains(joined, "existing pin") || !strings.Contains(joined, "parity skill body") {
		t.Fatalf("pins = %q, want existing pin and selected current-message skill", joined)
	}
}

func TestSessionSummaryContextBlockingStreamingParity(t *testing.T) {
	store := storagememory.New()
	ctx := storage.WithTenant(context.Background(), "tenant-a")
	now := time.Now().Add(-time.Hour)
	if err := store.CreateSession(ctx, &storage.Session{ID: "prior", AgentID: "coder", Status: "completed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateSession(prior) error = %v", err)
	}
	if err := store.AppendEvent(ctx, &storage.Event{ID: "prior-summary", SessionID: "prior", SeqNum: 1, Type: "chat_summary", Payload: map[string]any{"summary": "fix the parser carefully"}, CreatedAt: now}); err != nil {
		t.Fatalf("AppendEvent(prior) error = %v", err)
	}
	if err := store.CreateSession(ctx, &storage.Session{ID: "active", AgentID: "coder", Status: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateSession(active) error = %v", err)
	}
	if err := store.AppendEvent(ctx, &storage.Event{ID: "active-summary", SessionID: "active", SeqNum: 1, Type: "chat_summary", Payload: map[string]any{"summary": "active session secret"}, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AppendEvent(active) error = %v", err)
	}

	provider := &skillContextTestProvider{}
	a := &agent.Agent{ID: "coder", Model: provider, Storage: store, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine(), ContextPinsFn: func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
	}}
	setupSessionSummaries(session.NewManager(store, ""), map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", sessions: map[string]string{"coder": "active"}}

	blockingResult, err := orch.Execute(ctx, ExecutionRequest{Message: "fix parser", SessionID: "active"})
	if err != nil {
		t.Fatalf("blocking Execute() error = %v", err)
	}
	streamed, err := orch.Execute(ctx, ExecutionRequest{Message: "fix parser", SessionID: "active", Mode: ExecutionStreaming})
	if err != nil {
		t.Fatalf("streaming Execute() error = %v", err)
	}
	for range streamed.Stream {
	}

	blocking := strings.Join(systemContents(provider.request(0)), "\n")
	streaming := strings.Join(systemContents(provider.request(1)), "\n")
	if blocking != streaming {
		t.Fatalf("blocking pins %q differ from streaming pins %q", blocking, streaming)
	}
	if !strings.Contains(blocking, "existing pin") || !strings.Contains(blocking, "session=prior") || !strings.Contains(blocking, "fix the parser carefully") {
		t.Fatalf("summary pins = %q, want existing and prior-session pins", blocking)
	}
	if got, want := contextSource(blockingResult.ContextReport, ContextSourceSessionSummaries), contextSource(streamed.ContextReport, ContextSourceSessionSummaries); got != want || got.SelectedCount != 1 || got.Bytes == 0 {
		t.Fatalf("summary reports = (%#v, %#v), want equivalent selected metadata", got, want)
	}
	for _, message := range provider.request(0).Messages {
		if strings.HasPrefix(message.Content, "Relevant context from prior sessions:") && strings.Contains(message.Content, "active session secret") {
			t.Fatalf("prior-session pin contains active session: %q", message.Content)
		}
	}
}

func TestSessionSummaryContextOmitsUnavailableHistory(t *testing.T) {
	base := storagememory.New()
	a := &agent.Agent{ID: "coder", ContextPinsFn: func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
	}}
	setupSessionSummaries(session.NewManager(failingSummaryStorage{Storage: base}, ""), map[string]*agent.Agent{"coder": a})

	ctx := storage.WithSession(context.WithValue(context.Background(), messageKey{}, "query"), "active")
	got := strings.Join(messageContents(a.ContextPinsFn(ctx)), "\n")
	if got != "existing pin" {
		t.Fatalf("pins = %q, want unavailable history omitted without replacing existing pins", got)
	}
}

func TestMemoryPromptPinsUseCurrentContextTenant(t *testing.T) {
	t.Chdir(t.TempDir())
	a := &agent.Agent{ID: "coder"}
	store := setupMemory(&config.Config{Memory: config.MemoryConfig{Enabled: true}}, map[string]*agent.Agent{"coder": a})
	tenantAContext := storage.WithTenant(context.Background(), "tenant-a")
	tenantBContext := storage.WithTenant(context.Background(), "tenant-b")
	if _, err := store.ForContext(tenantAContext).Add(memory.CategoryProject, "shared query tenant A pin"); err != nil {
		t.Fatalf("tenant A Add: %v", err)
	}
	if _, err := store.ForContext(tenantBContext).Add(memory.CategoryProject, "shared query tenant B pin"); err != nil {
		t.Fatalf("tenant B Add: %v", err)
	}

	for _, tt := range []struct {
		name    string
		ctx     context.Context
		want    string
		notWant string
	}{
		{name: "tenant A recall", ctx: context.WithValue(tenantAContext, messageKey{}, "shared query"), want: "tenant A pin", notWant: "tenant B pin"},
		{name: "tenant B recall", ctx: context.WithValue(tenantBContext, messageKey{}, "shared query"), want: "tenant B pin", notWant: "tenant A pin"},
		{name: "tenant A recent", ctx: tenantAContext, want: "tenant A pin", notWant: "tenant B pin"},
		{name: "tenant B recent", ctx: tenantBContext, want: "tenant B pin", notWant: "tenant A pin"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pins := strings.Join(messageContents(a.ContextPinsFn(tt.ctx)), "\n")
			if !strings.Contains(pins, tt.want) || strings.Contains(pins, tt.notWant) {
				t.Fatalf("pins = %q, want %q and no %q", pins, tt.want, tt.notWant)
			}
		})
	}
}

func TestExecuteMemoryIntentBlockingStreamingParity(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(map[ExecutionMode]string{ExecutionBlocking: "blocking", ExecutionStreaming: "streaming"}[mode], func(t *testing.T) {
			t.Chdir(t.TempDir())
			provider := &skillContextTestProvider{}
			a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
			cfg := &config.Config{Memory: config.MemoryConfig{Enabled: true, AutoExtract: true}}
			store := setupMemory(cfg, map[string]*agent.Agent{"coder": a})
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", cfg: cfg, memory: store}
			ctx := storage.WithTenant(context.Background(), "tenant-a")
			const message = "remember project: run make test before release"

			result, err := orch.Execute(ctx, ExecutionRequest{Message: message, Mode: mode})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if mode == ExecutionStreaming {
				for range result.Stream {
				}
			}
			if result.MemoryIntent == nil || !result.MemoryIntent.Applied || result.MemoryIntent.Action != memory.IntentRemember || result.MemoryIntent.Category != memory.CategoryProject || result.MemoryIntent.RecordID == "" {
				t.Fatalf("MemoryIntent = %+v", result.MemoryIntent)
			}
			records, err := store.ForContext(ctx).List(memory.CategoryProject)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(records) != 1 || records[0].Content != "run make test before release" || !records[0].Validated || records[0].Source == "" || records[0].Repository == "" || records[0].Revision == "" || records[0].Fingerprint == "" {
				t.Fatalf("records = %+v, want one exact provenance-bearing payload", records)
			}
			if got := userContent(provider.request(0)); got != message {
				t.Fatalf("model message = %q, want original %q", got, message)
			}
			if pins := strings.Join(systemContents(provider.request(0)), "\n"); !strings.Contains(pins, "run make test before release") {
				t.Fatalf("same-turn memory pin = %q, want saved payload", pins)
			}
			memorySource := contextSource(result.ContextReport, ContextSourceMemory)
			if memorySource.SelectedCount != 1 || memorySource.Bytes == 0 || memorySource.OmissionReason != "" {
				t.Fatalf("memory context report = %#v", memorySource)
			}
		})
	}
}

func TestExecuteMemoryIntentUsesTenantForForget(t *testing.T) {
	provider := &skillContextTestProvider{}
	a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	cfg := &config.Config{Memory: config.MemoryConfig{Enabled: true, AutoExtract: true}}
	store := memory.NewStore(t.TempDir())
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", cfg: cfg, memory: store}
	tenantA := storage.WithTenant(context.Background(), "tenant-a")
	tenantB := storage.WithTenant(context.Background(), "tenant-b")
	record, err := store.ForContext(tenantA).Add(memory.CategoryUser, "tenant A preference")
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	message := "forget: " + record.ID

	result, err := orch.Execute(tenantB, ExecutionRequest{Message: message})
	if err == nil || !strings.Contains(err.Error(), record.ID) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-tenant forget error = %v", err)
	}
	if result.MemoryIntent == nil || result.MemoryIntent.Applied {
		t.Fatalf("failed forget result = %+v", result.MemoryIntent)
	}
	if records, listErr := store.ForContext(tenantA).List(""); listErr != nil || len(records) != 1 {
		t.Fatalf("tenant A records after cross-tenant forget = %+v, %v", records, listErr)
	}

	result, err = orch.Execute(tenantA, ExecutionRequest{Message: message, Mode: ExecutionStreaming})
	if err != nil {
		t.Fatalf("tenant A forget: %v", err)
	}
	for range result.Stream {
	}
	if result.MemoryIntent == nil || !result.MemoryIntent.Applied {
		t.Fatalf("successful forget result = %+v", result.MemoryIntent)
	}
	if records, listErr := store.ForContext(tenantA).List(""); listErr != nil || len(records) != 0 {
		t.Fatalf("tenant A records after forget = %+v, %v", records, listErr)
	}
}

func TestExecuteRecallPastUsesExplicitQueryWithoutMutatingMessage(t *testing.T) {
	t.Chdir(t.TempDir())
	provider := &skillContextTestProvider{}
	a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	cfg := &config.Config{Memory: config.MemoryConfig{Enabled: true, AutoExtract: true}}
	store := setupMemory(cfg, map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", cfg: cfg, memory: store}
	if _, err := store.Add(memory.CategoryProject, "parser decisions use anchored commands"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	const message = "recall-past: parser decisions"

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: message})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.MemoryIntent == nil || !result.MemoryIntent.Applied || result.MemoryIntent.Action != memory.IntentRecallPast {
		t.Fatalf("MemoryIntent = %+v", result.MemoryIntent)
	}
	if got := userContent(provider.request(0)); got != message {
		t.Fatalf("model message = %q, want original %q", got, message)
	}
	if pins := strings.Join(systemContents(provider.request(0)), "\n"); !strings.Contains(pins, "parser decisions use anchored commands") {
		t.Fatalf("recall-past pins = %q", pins)
	}
}

func TestExecuteMemoryIntentHonorsAutoExtractAndIgnoresIncidentalProse(t *testing.T) {
	for _, tt := range []struct {
		name        string
		autoExtract bool
		message     string
		wantResult  bool
		wantReason  string
	}{
		{name: "disabled explicit intent", message: "remember: use tabs", wantResult: true, wantReason: "auto_extract_disabled"},
		{name: "incidental always", autoExtract: true, message: "What do you always run before release?"},
		{name: "incidental remember question", autoExtract: true, message: "Can you remember what happened yesterday?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := &skillContextTestProvider{}
			a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
			cfg := &config.Config{Memory: config.MemoryConfig{Enabled: true, AutoExtract: tt.autoExtract}}
			store := memory.NewStore(t.TempDir())
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", cfg: cfg, memory: store}

			result, err := orch.Execute(context.Background(), ExecutionRequest{Message: tt.message})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if (result.MemoryIntent != nil) != tt.wantResult {
				t.Fatalf("MemoryIntent = %+v, want present %t", result.MemoryIntent, tt.wantResult)
			}
			if result.MemoryIntent != nil && (result.MemoryIntent.Applied || result.MemoryIntent.Reason != tt.wantReason) {
				t.Fatalf("MemoryIntent = %+v, want disabled reason %q", result.MemoryIntent, tt.wantReason)
			}
			if records, listErr := store.List(""); listErr != nil || len(records) != 0 {
				t.Fatalf("records = %+v, %v; want no write", records, listErr)
			}
		})
	}
}

func TestExecuteMemoryPersistenceErrorIsActionable(t *testing.T) {
	root := t.TempDir()
	blockingPath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &skillContextTestProvider{}
	a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	cfg := &config.Config{Memory: config.MemoryConfig{Enabled: true, AutoExtract: true}}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": a}, active: "coder", cfg: cfg,
		memory: memory.NewStore(filepath.Join(blockingPath, "memory")),
	}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "remember: exact payload"})
	if err == nil || !strings.Contains(err.Error(), "apply remember intent") || !strings.Contains(err.Error(), "memory: read feedback") || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Execute() error = %v, want actionable persistence error", err)
	}
	if result.MemoryIntent == nil || result.MemoryIntent.Action != memory.IntentRemember || result.MemoryIntent.Applied {
		t.Fatalf("MemoryIntent = %+v", result.MemoryIntent)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("provider called %d times after failed persistence", len(provider.requests))
	}
}

func TestLearnedPatternContextBlockingStreamingParity(t *testing.T) {
	provider := &skillContextTestProvider{}
	a := &agent.Agent{
		ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine(),
		ContextPinsFn: func(context.Context) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
		},
	}
	store := learning.NewStore(t.TempDir())
	// This hash is generated by approval callers from the same normalized
	// trigger; derive it through a candidate extraction to avoid fuzzy matching.
	candidate := learning.ClusterCandidates([]learning.SessionSegment{
		learnedTestSegment("/repo", "fix parser"),
		learnedTestSegment("/repo", "FIX-parser!"),
		learnedTestSegment("/repo", "fix parser"),
	}, learning.MinimumCandidateCount)[0]
	approved, err := store.ApprovePattern(candidate, "revision", learning.ReplayEvidence{
		VerifiedOutcomes: learning.MinimumCandidateCount, QualityPassed: true, PolicyPassed: true,
	}, learning.MinimumCandidateCount)
	if err != nil {
		t.Fatalf("ApprovePattern() error = %v", err)
	}
	setupLearnedPatternPins(store, "/repo", "revision", map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", sessions: map[string]string{"coder": "session-1"}}

	if _, err := orch.Chat(context.Background(), "FIX parser!"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	stream, err := orch.ChatStream(context.Background(), "FIX parser!")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}
	blocking := strings.Join(systemContents(provider.request(0)), "\n")
	streaming := strings.Join(systemContents(provider.request(1)), "\n")
	if blocking != streaming || !strings.Contains(blocking, "existing pin") || !strings.Contains(blocking, "Pattern version "+fmt.Sprint(approved.Version)) || !strings.Contains(blocking, "advisory only") {
		t.Fatalf("blocking pins %q, streaming pins %q; want equivalent composed advisory", blocking, streaming)
	}
}

func TestLearnedPatternContextOmitsStaleNonmatchingAndMalformedStore(t *testing.T) {
	for _, tt := range []struct {
		name, repo, trigger, revision string
		malformed, missing            bool
	}{
		{name: "stale revision", repo: "/repo", trigger: "fix parser", revision: "other"},
		{name: "other repository", repo: "/other", trigger: "fix parser", revision: "revision"},
		{name: "other trigger", repo: "/repo", trigger: "fix lexer", revision: "revision"},
		{name: "missing optional store", repo: "/repo", trigger: "fix parser", revision: "revision", missing: true},
		{name: "malformed optional store", repo: "/repo", trigger: "fix parser", revision: "revision", malformed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store := learning.NewStore(dir)
			if tt.malformed {
				if err := os.WriteFile(filepath.Join(dir, "pattern-versions.yaml"), []byte("patterns: ["), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if !tt.missing {
				candidate := learning.ClusterCandidates([]learning.SessionSegment{
					learnedTestSegment("/repo", "fix parser"), learnedTestSegment("/repo", "fix parser"), learnedTestSegment("/repo", "fix parser"),
				}, learning.MinimumCandidateCount)[0]
				if _, err := store.ApprovePattern(candidate, "revision", learning.ReplayEvidence{VerifiedOutcomes: 3, QualityPassed: true, PolicyPassed: true}, 3); err != nil {
					t.Fatal(err)
				}
			}
			a := &agent.Agent{ID: "coder", ContextPinsFn: func(context.Context) []model.Message {
				return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
			}}
			setupLearnedPatternPins(store, tt.repo, tt.revision, map[string]*agent.Agent{"coder": a})
			ctx := context.WithValue(context.Background(), messageKey{}, tt.trigger)
			if got := strings.Join(messageContents(a.ContextPinsFn(ctx)), "\n"); got != "existing pin" {
				t.Fatalf("pins = %q, want optional learned pattern omitted", got)
			}
		})
	}
}

func TestLearnedPatternAdvisoryDoesNotBypassApprovalOrSecurity(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	store := learning.NewStore(t.TempDir())
	candidate := learning.ClusterCandidates([]learning.SessionSegment{
		learnedTestSegment("/repo", "deploy release"), learnedTestSegment("/repo", "deploy release"), learnedTestSegment("/repo", "deploy release"),
	}, learning.MinimumCandidateCount)[0]
	if _, err := store.ApprovePattern(candidate, "revision", learning.ReplayEvidence{VerifiedOutcomes: 3, QualityPassed: true, PolicyPassed: true}, 3); err != nil {
		t.Fatal(err)
	}
	setupLearnedPatternPins(store, "/repo", "revision", map[string]*agent.Agent{"coder": a})
	ctx := context.WithValue(context.Background(), messageKey{}, "deploy release")
	if pins := strings.Join(messageContents(a.ContextPinsFn(ctx)), "\n"); !strings.Contains(pins, "does not authorize tools") {
		t.Fatalf("learned advisory pin = %q", pins)
	}

	executions, approvals := 0, 0
	registerApprovalTool(a.Tools, "shell", &executions)
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(ctx, "shell", map[string]any{"command": "git push origin main"}); err == nil {
		t.Fatal("advisory pattern bypassed required approval")
	}
	if _, err := a.Tools.Execute(ctx, "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("advisory pattern bypassed security denial")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
}

func learnedTestSegment(repo, trigger string) learning.SessionSegment {
	return learning.SessionSegment{
		RepoPath: repo, Trigger: learning.Turn{Content: trigger},
		Turns:     []learning.Turn{{Role: "assistant", Content: "inspect before editing"}},
		ToolCalls: []learning.ToolCall{{Name: "file_read"}, {Name: "file_write"}},
		Outcome:   &learning.Outcome{Kind: "accepted", Timestamp: time.Now()},
	}
}

func TestWithSkillPinsExactSkill(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeTestSkill(t, root, "explicit-skill", "unlikely-trigger", "explicit skill body")

	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	catalog := setupSkills(&config.Config{}, root, map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", skillCatalog: catalog}
	ctx, err := orch.WithSkill(context.Background(), "EXPLICIT-SKILL")
	if err != nil {
		t.Fatalf("WithSkill() error = %v", err)
	}
	ctx = context.WithValue(ctx, messageKey{}, "unrelated request")
	pins := a.ContextPinsFn(ctx)
	if got := strings.Join(systemContents(&model.ChatRequest{Messages: pins}), "\n"); !strings.Contains(got, "explicit skill body") {
		t.Fatalf("explicit skill was not pinned: %q", got)
	}
	if _, err := orch.WithSkill(context.Background(), "missing"); err == nil {
		t.Fatal("WithSkill() accepted an unknown skill")
	}
}

func TestLSPToolsAndDiagnosticPinsPreserveParityAndBounds(t *testing.T) {
	root := t.TempDir()
	files := []string{"internal/a.go", "internal/b.go", "other/a.go"}
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder", Tools: tool.NewRegistry()},
		"reviewer": {ID: "reviewer", Tools: tool.NewRegistry()},
	}
	for _, a := range agents {
		a.ContextPinsFn = func(context.Context) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
		}
	}
	called := make(map[string]int)
	diagnostics := &tool.Definition{
		Name: "lsp_diagnostics",
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			file := args["file"].(string)
			called[file]++
			items := make([]map[string]any, 0, 6)
			for i := 1; i <= 6; i++ {
				severity := "warning"
				if i <= 2 {
					severity = "error"
				}
				items = append(items, map[string]any{"severity": severity, "message": "diagnostic", "line": i, "col": 1})
			}
			return map[string]any{"diagnostics": items, "count": len(items)}, nil
		},
	}
	definitions := []*tool.Definition{
		diagnostics,
		{Name: "lsp_hover"},
		{Name: "lsp_references"},
		{Name: "lsp_rename_preview"},
	}
	installLSPTools(root, files, agents, definitions)

	for id, a := range agents {
		for _, name := range []string{"lsp_diagnostics", "lsp_hover", "lsp_references", "lsp_rename_preview"} {
			if _, ok := a.Tools.Get(name); !ok {
				t.Fatalf("agent %s lacks tool %s", id, name)
			}
		}
		ctx := context.WithValue(context.Background(), messageKey{}, "fix `internal/b.go`; a.go is ambiguous")
		joined := strings.Join(messageContents(a.ContextPinsFn(ctx)), "\n")
		if !strings.Contains(joined, "existing pin") || !strings.Contains(joined, "errors: 2, warnings: 4; showing 5 of 6") {
			t.Fatalf("agent %s pins = %q", id, joined)
		}
		if strings.Count(joined, "\n- internal/b.go:") != maxPinnedLSPDiagnostics {
			t.Fatalf("agent %s diagnostics were not bounded to %d: %q", id, maxPinnedLSPDiagnostics, joined)
		}
	}
	if called["internal/b.go"] != len(agents) || called["internal/a.go"] != 0 || called["other/a.go"] != 0 {
		t.Fatalf("diagnostic calls = %#v, want only unambiguous internal/b.go", called)
	}
}

func TestLSPDiagnosticPinsIgnoreUnavailableServer(t *testing.T) {
	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
	a.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
	}
	installLSPTools("/workspace", []string{"main.go"}, map[string]*agent.Agent{"coder": a}, []*tool.Definition{{
		Name: "lsp_diagnostics",
		Handler: func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("lsp: no language server available")
		},
	}})
	pins := a.ContextPinsFn(context.WithValue(context.Background(), messageKey{}, "check main.go"))
	if got := strings.Join(messageContents(pins), "\n"); got != "existing pin" {
		t.Fatalf("pins = %q, want missing server to preserve existing pins only", got)
	}
}

func TestLSPDiagnosticContextBlockingStreamingParity(t *testing.T) {
	provider := &skillContextTestProvider{}
	a := &agent.Agent{
		ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine(),
	}
	installLSPTools("/workspace", []string{"main.go"}, map[string]*agent.Agent{"coder": a}, []*tool.Definition{{
		Name: "lsp_diagnostics",
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"diagnostics": []map[string]any{{
				"severity": "error", "message": "broken", "line": 2, "col": 3,
			}}}, nil
		},
	}})
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": a}, active: "coder",
		sessions: map[string]string{"coder": "session-1"},
	}

	if _, err := orch.Chat(context.Background(), "fix main.go"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	stream, err := orch.ChatStream(context.Background(), "fix main.go")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}
	blocking := strings.Join(systemContents(provider.request(0)), "\n")
	streaming := strings.Join(systemContents(provider.request(1)), "\n")
	if blocking != streaming || !strings.Contains(blocking, "main.go:2:3 [error] broken") {
		t.Fatalf("blocking pins %q, streaming pins %q; want equivalent diagnostics", blocking, streaming)
	}
}

func TestSkillToolHistoryIsBoundedAndSessionScoped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	toolNames := []string{"toolalpha", "toolbravo", "toolcharlie", "tooldelta", "toolecho", "toolfoxtrot"}
	for _, name := range toolNames {
		writeTestSkill(t, root, name+"-skill", name, name+" body")
	}

	a := &agent.Agent{ID: "coder"}
	setupSkills(&config.Config{}, root, map[string]*agent.Agent{"coder": a})
	if len(a.Hooks) != 1 {
		t.Fatalf("skill hooks = %d, want 1", len(a.Hooks))
	}
	sessionOne := storage.WithSession(context.Background(), "session-1")
	for _, name := range toolNames {
		if err := a.Hooks.After(sessionOne, &hooks.Event{Type: hooks.EventToolCallAfter, Name: name}); err != nil {
			t.Fatalf("After(%q) error = %v", name, err)
		}
	}

	pins := a.ContextPinsFn(context.WithValue(sessionOne, messageKey{}, "continue"))
	rendered := strings.Join(messageContents(pins), "\n")
	if strings.Contains(rendered, "toolalpha body") {
		t.Fatalf("oldest tool influenced selection after history limit: %q", rendered)
	}
	if count := strings.Count(rendered, "<skill name="); count != 3 {
		t.Fatalf("selected skill count = %d, want top-K limit 3; pins = %q", count, rendered)
	}

	sessionTwo := storage.WithSession(context.Background(), "session-2")
	otherPins := a.ContextPinsFn(context.WithValue(sessionTwo, messageKey{}, "continue"))
	otherRendered := strings.Join(messageContents(otherPins), "\n")
	for _, name := range toolNames {
		if strings.Contains(otherRendered, name+" body") {
			t.Fatalf("session-1 tool %q leaked into session-2 pins: %q", name, otherRendered)
		}
	}
}

func systemContents(req *model.ChatRequest) []string {
	var contents []string
	for _, msg := range req.Messages {
		if msg.Role == model.RoleSystem {
			contents = append(contents, msg.Content)
		}
	}
	return contents
}

func messageContents(messages []model.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}
	return contents
}

func TestUserHookPolicyDenialPrecedesMiddleware(t *testing.T) {
	root := t.TempDir()
	runner, err := security.NewHookRunner(root)
	if err != nil {
		t.Fatalf("NewHookRunner() error = %v", err)
	}
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerApprovalTool(a.Tools, "shell", &executions)
	wrapUserToolHooks(a, config.HooksConfig{PreToolCall: []config.HookDef{{
		Name: "pre", Command: "touch hook-ran", TimeoutMs: 1000,
	}}}, runner)

	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("policy-denied tool call succeeded")
	}
	if executions != 0 {
		t.Fatalf("handler executions = %d, want 0", executions)
	}
	if _, err := os.Stat(filepath.Join(root, "hook-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-hook ran before policy denial: %v", err)
	}
}

func TestUserHookPreFailureBlocksHandlerInOrder(t *testing.T) {
	root := t.TempDir()
	runner, err := security.NewHookRunner(root)
	if err != nil {
		t.Fatalf("NewHookRunner() error = %v", err)
	}
	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
	executions := 0
	registerPermissionTool(a.Tools, "test", tool.PermAllow, &executions)
	wrapUserToolHooks(a, config.HooksConfig{PreToolCall: []config.HookDef{
		{Name: "first", Command: "printf first >> order", TimeoutMs: 1000},
		{Name: "block", Command: "printf second >> order; exit 7", TimeoutMs: 1000},
		{Name: "third", Command: "printf third >> order", TimeoutMs: 1000},
	}}, runner)

	if _, err := a.Tools.Execute(context.Background(), "test", nil); !errors.Is(err, security.ErrHookExit) {
		t.Fatalf("Execute() error = %v, want hook exit error", err)
	}
	if executions != 0 {
		t.Fatalf("handler executions = %d, want 0", executions)
	}
	data, err := os.ReadFile(filepath.Join(root, "order"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := string(data); got != "firstsecond" {
		t.Fatalf("hook order = %q, want firstsecond", got)
	}
}

func TestUserHookPostSeesRawOutputBeforeCompressionAndCannotMaskResult(t *testing.T) {
	root := t.TempDir()
	runner, err := security.NewHookRunner(root)
	if err != nil {
		t.Fatalf("NewHookRunner() error = %v", err)
	}
	raw := strings.Repeat("raw-output-", 100)
	a := &agent.Agent{
		ID:      "coder",
		Model:   &routingTestProvider{provider: "test", model: "test"},
		Tools:   tool.NewRegistry(),
		Storage: storagememory.New(),
	}
	a.Tools.Register(&tool.Definition{
		Name: "test", Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) { return raw, nil },
	})
	wrapUserToolHooks(a, config.HooksConfig{PostToolCall: []config.HookDef{
		{Name: "inspect", Command: "printf '%s' {{tool_output}} > raw.json", TimeoutMs: 1000},
		{Name: "fail", Command: "exit 9", TimeoutMs: 1000},
	}}, runner)
	toolcompress.Wrap(a, 1)

	result, err := a.Tools.Execute(storage.WithSession(context.Background(), "session-1"), "test", nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	compressed, ok := result.(map[string]any)
	if !ok || compressed["compressed"] != true {
		t.Fatalf("result = %#v, want compressed result despite failing post-hook", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "raw.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(data), `"`+raw+`"`; got != want {
		t.Fatalf("post-hook output = %q, want raw JSON output", got)
	}
}

func TestHookPromptBlockingStreamingParityIsBoundedAndInjectedOnce(t *testing.T) {
	root := t.TempDir()
	runner, err := security.NewHookRunner(root)
	if err != nil {
		t.Fatalf("NewHookRunner() error = %v", err)
	}
	provider := &skillContextTestProvider{}
	a := &agent.Agent{
		ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine(),
	}
	cfg := &config.Config{Hooks: config.HooksConfig{UserPromptSubmit: []config.HookDef{{
		Name:      "context",
		Command:   "printf x >> prompt-runs; i=0; while [ $i -lt 5000 ]; do printf x; i=$((i+1)); done; printf '\\n%s|%s|%s' {{session_id}} {{agent_id}} {{user_message}}",
		TimeoutMs: 2000,
	}}}}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": a}, active: "coder",
		sessions: map[string]string{"coder": "session-1"}, cfg: cfg, hookRunner: runner,
	}

	if _, err := orch.Chat(context.Background(), "hello"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	stream, err := orch.ChatStream(context.Background(), "hello")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}

	blocking := userContent(provider.request(0))
	streaming := userContent(provider.request(1))
	if blocking != streaming {
		t.Fatalf("blocking prompt differs from streaming prompt")
	}
	if strings.Count(blocking, "<user_hook_context>") != 1 || strings.Count(blocking, "</user_hook_context>") != 1 {
		t.Fatalf("prompt context was not injected exactly once: %q", blocking)
	}
	start := strings.Index(blocking, "<user_hook_context>\n") + len("<user_hook_context>\n")
	end := strings.Index(blocking, "\n</user_hook_context>")
	if start < len("<user_hook_context>\n") || end < start {
		t.Fatalf("prompt context delimiters missing: %q", blocking)
	}
	if got := len(blocking[start:end]); got > userHookPromptContextTokens*security.CaptureBytesPerToken {
		t.Fatalf("hook context bytes = %d, want at most %d", got, userHookPromptContextTokens*security.CaptureBytesPerToken)
	}
	if !strings.Contains(blocking[start:end], "session-1|coder|hello") {
		t.Fatalf("hook context lacks session/agent/message variables: %q", blocking[start:end])
	}
	runs, err := os.ReadFile(filepath.Join(root, "prompt-runs"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := string(runs); got != "xx" {
		t.Fatalf("prompt hook runs = %q, want once per chat path", got)
	}
}

func userContent(req *model.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == model.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

type orchestratorMCPClient struct {
	closed int
	calls  int
}

func (*orchestratorMCPClient) Connect(context.Context) error { return nil }
func (*orchestratorMCPClient) ListTools(context.Context) ([]mcp.ToolInfo, error) {
	return []mcp.ToolInfo{{Name: "read"}}, nil
}
func (c *orchestratorMCPClient) CallTool(context.Context, string, map[string]any) (any, error) {
	c.calls++
	return "ok", nil
}
func (c *orchestratorMCPClient) Close() error { c.closed++; return nil }

func TestSetupMCPRuntimesOwnsIndependentClientsAndYoloStillRequiresApproval(t *testing.T) {
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder", Tools: tool.NewRegistry()},
		"reviewer": {ID: "reviewer", Tools: tool.NewRegistry()},
	}
	policy := &security.Policy{TrustedMCPServers: []string{"filesystem"}, MCPDefaultPermission: security.MCPRequireApproval}
	var clients []*orchestratorMCPClient
	runtimes := setupMCPRuntimes(context.Background(), agents, []mcp.ServerConfig{{
		Name: "filesystem", Transport: mcp.TransportStdio, Command: "server",
	}}, policy, time.Second, func(mcp.ServerConfig) (mcpdiscover.RuntimeClient, error) {
		client := &orchestratorMCPClient{}
		clients = append(clients, client)
		return client, nil
	})
	if len(runtimes) != 2 || len(clients) != 2 || clients[0] == clients[1] {
		t.Fatalf("runtimes=%d clients=%#v, want independent clients", len(runtimes), clients)
	}
	normalizeToolPermissions(agents)
	orch := &Orchestrator{
		agents: agents, mcpRuntimes: runtimes,
		permissionChecker: security.NewPermissionChecker(policy, "/workspace"),
	}
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return true, nil
	})
	if err := orch.SetPermissionMode("auto_approve"); err != nil {
		t.Fatalf("SetPermissionMode() error = %v", err)
	}
	name := mcpdiscover.ToolName("filesystem", "read")
	for _, a := range agents {
		if _, err := a.Tools.Execute(context.Background(), name, nil); err != nil {
			t.Fatalf("Execute(%s) error = %v", a.ID, err)
		}
	}
	if approvals != 2 {
		t.Fatalf("approval calls = %d, want 2 under yolo", approvals)
	}
	if err := orch.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := orch.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	for i, client := range clients {
		if client.closed != 1 || client.calls != 1 {
			t.Errorf("client %d closed=%d calls=%d, want 1/1", i, client.closed, client.calls)
		}
	}
}

func TestConnectMCPApprovesDiscoveredServer(t *testing.T) {
	agents := map[string]*agent.Agent{
		"coder": {ID: "coder", Tools: tool.NewRegistry()},
	}
	policy := &security.Policy{MCPDefaultPermission: security.MCPRequireApproval}
	client := &orchestratorMCPClient{}
	runtimes := setupMCPRuntimes(context.Background(), agents, []mcp.ServerConfig{{
		Name: "filesystem", Transport: mcp.TransportStdio, Command: "server",
	}}, policy, time.Second, func(mcp.ServerConfig) (mcpdiscover.RuntimeClient, error) {
		return client, nil
	})
	orch := &Orchestrator{
		agents:            agents,
		policy:            policy,
		mcpRuntimes:       runtimes,
		mcpFactory:        func(mcp.ServerConfig) (mcpdiscover.RuntimeClient, error) { return client, nil },
		permissionChecker: security.NewPermissionChecker(policy, "/workspace"),
	}
	if statuses := orch.MCPStatuses(); len(statuses) != 1 || statuses[0].State != mcpdiscover.StateApprovalRequired {
		t.Fatalf("MCPStatuses() = %#v", statuses)
	}
	status, err := orch.ConnectMCP(context.Background(), "filesystem")
	if err != nil {
		t.Fatalf("ConnectMCP() error = %v", err)
	}
	if status.State != mcpdiscover.StateConnected || status.Tools != 1 {
		t.Fatalf("ConnectMCP() status = %#v", status)
	}
	name := mcpdiscover.ToolName("filesystem", "read")
	if _, err := agents["coder"].Tools.Execute(context.Background(), name, nil); err == nil {
		t.Fatal("connected MCP tool executed without approval")
	}
}

func TestSetupSecurityUsesFloorWhenOverlaysAreMissing(t *testing.T) {
	agents := map[string]*agent.Agent{"coder": {ID: "coder"}}
	policy, err := setupSecurity(t.TempDir(), t.TempDir(), "/workspace", nil, agents)
	if err != nil {
		t.Fatalf("setupSecurity() error = %v", err)
	}
	if len(policy.DeniedPaths) == 0 || policy.MCPDefaultPermission != security.MCPRequireApproval {
		t.Fatalf("setupSecurity() returned non-floor policy: %#v", policy)
	}
	if len(agents["coder"].Hooks) != 1 {
		t.Fatalf("guard hooks = %d, want 1", len(agents["coder"].Hooks))
	}
}

func TestSetupSecurityRejectsMalformedOrWeakeningOverlayBeforeInstallingGuard(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"malformed", "shell: [", "parse policy yaml"},
		{"weakening", "filesystem:\n  writable_paths: ['..']\n", "filesystem.writable_paths[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(projectDir, "security.yaml"), []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			agents := map[string]*agent.Agent{"coder": {ID: "coder"}}
			policy, err := setupSecurity(projectDir, t.TempDir(), "/workspace", nil, agents)
			if err == nil || policy != nil {
				t.Fatalf("setupSecurity() = %#v, %v; want nil policy and error", policy, err)
			}
			if !strings.Contains(err.Error(), "project security overlay") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("setupSecurity() error = %q, want source and %q", err, tc.want)
			}
			if len(agents["coder"].Hooks) != 0 {
				t.Fatal("guard installed after invalid policy")
			}
		})
	}
}

func TestSecurityFloorAppliesToYoloForEveryConfiguredAgent(t *testing.T) {
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder", Tools: tool.NewRegistry()},
		"reviewer": {ID: "reviewer", Tools: tool.NewRegistry()},
	}
	policy, err := setupSecurity(t.TempDir(), t.TempDir(), "/workspace", nil, agents)
	if err != nil {
		t.Fatalf("setupSecurity() error = %v", err)
	}
	executions := 0
	for _, a := range agents {
		registerPermissionTool(a.Tools, "shell", tool.PermAllow, &executions)
	}
	normalizeToolPermissions(agents)
	orch := &Orchestrator{agents: agents, permissionChecker: security.NewPermissionChecker(policy, "/workspace")}
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		return true, nil
	})
	if err := orch.SetPermissionMode("auto_approve"); err != nil {
		t.Fatalf("SetPermissionMode() error = %v", err)
	}
	for id, a := range agents {
		if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "sudo true"}); err == nil {
			t.Errorf("%s executed floor-denied command under yolo", id)
		}
	}
	if executions != 0 {
		t.Fatalf("floor-denied handler executions = %d, want 0", executions)
	}
}

func TestSecurityGuardDenialIsRecoverableByAgent(t *testing.T) {
	provider := &recoverableDenialProvider{}
	a, err := agent.New("coder", "Coder").WithModel(provider).Build()
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	registerPermissionTool(a.Tools, "file_read", tool.PermAllow, &executions)
	agents := map[string]*agent.Agent{"coder": a}
	if _, err := setupSecurity(t.TempDir(), t.TempDir(), "/workspace", nil, agents); err != nil {
		t.Fatalf("setupSecurity() error = %v", err)
	}
	response, err := a.Chat(context.Background(), "read the environment")
	if err != nil {
		t.Fatalf("Chat() error = %v, want recoverable denial", err)
	}
	if response.Content != "recovered" || executions != 0 || len(provider.requests) != 2 {
		t.Fatalf("recovery = content %q, executions %d, requests %d", response.Content, executions, len(provider.requests))
	}
	messages := provider.requests[1].Messages
	if len(messages) == 0 || messages[len(messages)-1].Role != model.RoleTool || !strings.Contains(messages[len(messages)-1].Content, "denied by policy") {
		t.Fatalf("follow-up messages = %#v, want recoverable policy-denial tool result", messages)
	}
}

func newPermissionTestOrchestrator(t *testing.T, store storage.Storage) *Orchestrator {
	t.Helper()
	policy, err := security.LoadPolicy([]byte(`
shell:
  allowed_commands: ["go", "git", "rm"]
  auto_allow: ['^go test']
  confirm: ['^git push']
  never_allow: ['^rm dangerous$']
`))
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder", Tools: tool.NewRegistry()},
		"reviewer": {ID: "reviewer", Tools: tool.NewRegistry()},
	}
	orch := &Orchestrator{
		agents:            agents,
		store:             store,
		permissionChecker: security.NewPermissionChecker(policy, "/workspace"),
	}
	orch.SetApprovalHandler(nil)
	return orch
}

func registerApprovalTool(registry *tool.Registry, name string, executions *int) {
	registerPermissionTool(registry, name, tool.PermRequireApproval, executions)
}

func registerPermissionTool(registry *tool.Registry, name string, permission tool.Permission, executions *int) {
	registry.Register(&tool.Definition{
		Name:       name,
		Permission: permission,
		Handler: func(context.Context, map[string]any) (any, error) {
			(*executions)++
			return "executed", nil
		},
	})
}

func TestNormalizeAllowShellAppliesConfirmAndNeverAllowPolicy(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "shell", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
		t.Fatal("originally allowed git push executed after human rejection")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("git push counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("originally allowed never_allow command executed")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("never_allow counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
}

func TestNormalizeAllowUnknownToolRequiresConfirmation(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "mcp_unknown", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "mcp_unknown", nil); err == nil {
		t.Fatal("originally allowed unknown tool executed after human rejection")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
}

func TestNormalizeAllowReadOnlyToolExecutesWithoutHumanCallback(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "file_read", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "README.md"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if approvals != 0 || executions != 1 {
		t.Fatalf("counts = approvals %d, executions %d; want 0, 1", approvals, executions)
	}
}

func TestNormalizeLeavesDeniedToolDenied(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "blocked", tool.PermDeny, &executions)
	normalizeToolPermissions(orch.agents)

	def, ok := a.Tools.Get("blocked")
	if !ok || def.Permission != tool.PermDeny {
		t.Fatalf("permission = %q, found %v; want %q", def.Permission, ok, tool.PermDeny)
	}
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return true, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "blocked", nil); err == nil {
		t.Fatal("denied tool executed")
	}
	if approvals != 0 || executions != 0 {
		t.Fatalf("counts = approvals %d, executions %d; want 0, 0", approvals, executions)
	}
}

func TestApprovalCompositionAppliesToEveryAgent(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})

	for id, a := range orch.agents {
		executions := 0
		registerApprovalTool(a.Tools, "shell", &executions)
		if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "go test ./..."}); err != nil {
			t.Fatalf("%s auto Execute() error = %v", id, err)
		}
		if approvals != 0 || executions != 1 {
			t.Fatalf("%s auto counts = approvals %d, executions %d; want 0, 1", id, approvals, executions)
		}
		if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
			t.Fatalf("%s confirm Execute() succeeded after rejection", id)
		}
		if approvals != 1 || executions != 1 {
			t.Fatalf("%s confirm counts = approvals %d, executions %d; want 1, 1", id, approvals, executions)
		}
		approvals = 0
	}
}

func TestAutoApproveModePreservesConfirmAndDeny(t *testing.T) {
	store := storagememory.New()
	orch := newPermissionTestOrchestrator(t, store)
	a := orch.agents["coder"]
	executions := 0
	registerApprovalTool(a.Tools, "shell", &executions)
	registerApprovalTool(a.Tools, "file_write", &executions)
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if err := orch.SetPermissionMode("auto_approve"); err != nil {
		t.Fatalf("SetPermissionMode() error = %v", err)
	}
	if got := a.Tools.PermissionMode(); got != tool.PermissionModePrompt {
		t.Fatalf("registry permission mode = %q, want prompt so policy is not bypassed", got)
	}

	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "go test ./..."}); err != nil {
		t.Fatalf("auto Execute() error = %v", err)
	}
	if _, err := a.Tools.Execute(context.Background(), "file_write", map[string]any{"path": "internal/new.go"}); err != nil {
		t.Fatalf("yolo file_write Execute() error = %v", err)
	}
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
		t.Fatal("explicit confirm command executed after rejection")
	}
	ctx := storage.WithSession(context.Background(), "session-1")
	if _, err := a.Tools.Execute(ctx, "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("never_allow command executed")
	}
	if approvals != 1 || executions != 2 {
		t.Fatalf("counts = approvals %d, executions %d; want 1, 2", approvals, executions)
	}
	logs, err := store.ListAuditLogs(ctx, "session-1", 10, 0)
	if err != nil {
		t.Fatalf("ListAuditLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Action != "block" || logs[0].Resource != "shell" {
		t.Fatalf("audit logs = %#v, want one shell block", logs)
	}
}

func TestApprovalHandlerReplacementRetainsPolicyComposition(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerApprovalTool(a.Tools, "shell", &executions)
	first, second := 0, 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		first++
		return true, nil
	})
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		second++
		return true, nil
	})

	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if first != 0 || second != 1 || executions != 1 {
		t.Fatalf("counts = first %d, second %d, executions %d; want 0, 1, 1", first, second, executions)
	}
}
