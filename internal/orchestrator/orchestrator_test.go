package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/toolcompress"
)

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
		t.Fatalf("provider calls = %d, want 1", got)
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
