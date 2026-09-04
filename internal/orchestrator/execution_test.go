package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/activation"
	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/verification"
)

type executionTestProvider struct {
	name    string
	modelID string

	mu       sync.Mutex
	requests []*model.ChatRequest
	contexts []context.Context
}

type executionContextHook struct {
	context context.Context
}

func (h *executionContextHook) Before(ctx context.Context, _ *hooks.Event) error {
	h.context = ctx
	return nil
}

func (h *executionContextHook) After(context.Context, *hooks.Event) error { return nil }

func (p *executionTestProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.record(ctx, req)
	return &model.ChatResponse{Role: model.RoleAssistant, Content: p.name}, nil
}

func (p *executionTestProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	p.record(ctx, req)
	responses := make(chan *model.ChatResponse, 1)
	responses <- &model.ChatResponse{Role: model.RoleAssistant, Content: p.name}
	close(responses)
	return responses, nil
}

func (p *executionTestProvider) Name() string  { return p.name }
func (p *executionTestProvider) Model() string { return p.modelID }

func (p *executionTestProvider) record(ctx context.Context, req *model.ChatRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contexts = append(p.contexts, ctx)
	p.requests = append(p.requests, req)
}

func (p *executionTestProvider) request(index int) *model.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[index]
}

func (p *executionTestProvider) executionContext(index int) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contexts[index]
}

func newExecutionTestAgent(id string, provider model.Provider) *agent.Agent {
	return &agent.Agent{
		ID:         id,
		Model:      provider,
		Tools:      tool.NewRegistry(),
		Guardrails: guardrails.NewEngine(),
	}
}

func TestExecuteKeepsActiveAgentAndAppliesModel(t *testing.T) {
	coder := &executionTestProvider{name: "coder", modelID: "coder-model"}
	debugger := &executionTestProvider{name: "debugger", modelID: "debugger-model"}
	routed := &executionTestProvider{name: "routed", modelID: "high-debug"}
	cfg := &router.Config{
		IntentRouting: []router.IntentRoute{{Intent: "debug", Agent: "debugger", Patterns: []string{"fix"}}},
		ModelRouting: router.ModelRouting{Models: map[router.Complexity]map[router.TaskKind]router.ModelSpec{
			router.ComplexityHigh: {router.TaskKindDebug: {Provider: "routed", Model: "high-debug"}},
		}},
	}
	rt, err := router.New(cfg, "coder")
	if err != nil {
		t.Fatalf("router.New() error = %v", err)
	}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{
			"coder":    newExecutionTestAgent("coder", coder),
			"debugger": newExecutionTestAgent("debugger", debugger),
		},
		active:         "coder",
		primary:        "coder",
		router:         rt,
		routingConfig:  cfg,
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
		buildProvider: func(agent.ModelConfig) (model.Provider, error) {
			return routed, nil
		},
	}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "fix this bug across multiple files"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.AgentID != "coder" || result.Response == nil || result.Response.Content != "routed" {
		t.Fatalf("Execute() result = %#v, want routed primary response", result)
	}
	if len(debugger.requests) != 0 {
		t.Fatalf("debugger requests = %d, want 0 (specialists are advisory)", len(debugger.requests))
	}
	if got := orch.agents["coder"].Model; got.Name() != "routed" || got.Model() != "high-debug" {
		t.Fatalf("primary model = (%q, %q), want (routed, high-debug)", got.Name(), got.Model())
	}
	req := routed.request(0)
	if req == nil {
		t.Fatal("expected a model request on the routed primary provider")
	}
	joined := ""
	for _, msg := range req.Messages {
		joined += msg.Content
	}
	if !strings.Contains(joined, "spawn_subagent debugger") || !strings.Contains(joined, "Path: complexity=") {
		t.Fatalf("prompt missing specialist or path hint: %q", joined)
	}
}

func TestExecutePreparesPredictiveContextForBothModes(t *testing.T) {
	store, err := graph.OpenStore(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertFileHash(ctx, "main.go", "revision"); err != nil {
		t.Fatalf("UpsertFileHash() error = %v", err)
	}
	if err := store.InsertSymbol(ctx, graph.Symbol{Name: "BuildAgent", Kind: graph.KindFunc, Package: "main", File: "main.go", Line: 1}); err != nil {
		t.Fatalf("InsertSymbol() error = %v", err)
	}
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	orch := &Orchestrator{
		agents:     map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)},
		active:     "coder",
		graphStore: store,
		actBuf:     activation.NewBuffer(1),
	}

	blockingResult, err := orch.Execute(ctx, ExecutionRequest{Message: "fix BuildAgent"})
	if err != nil {
		t.Fatalf("blocking Execute() error = %v", err)
	}
	result, err := orch.Execute(ctx, ExecutionRequest{Message: "fix BuildAgent", Mode: ExecutionStreaming})
	if err != nil {
		t.Fatalf("streaming Execute() error = %v", err)
	}
	for range result.Stream {
	}

	blocking := userContent(provider.request(0))
	streaming := userContent(provider.request(1))
	if blocking != streaming || !contains(blocking, "[Pre-loaded context]") {
		t.Fatalf("prepared prompts = (%q, %q), want equivalent predictive context", blocking, streaming)
	}
	if !reflect.DeepEqual(blockingResult.ContextReport, result.ContextReport) {
		t.Fatalf("blocking context report %#v differs from streaming %#v", blockingResult.ContextReport, result.ContextReport)
	}
	graphSource := contextSource(result.ContextReport, ContextSourceGraphPrediction)
	if graphSource.SelectedCount == 0 || graphSource.Bytes == 0 || graphSource.OmissionReason != "" {
		t.Fatalf("graph context report = %#v", graphSource)
	}
}

func TestExecutePropagatesTaskSessionAndPolicyContext(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	a := newExecutionTestAgent("coder", provider)
	hook := &executionContextHook{}
	a.Hooks = append(a.Hooks, hook)
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}
	policy := map[string]any{"approval": "required"}

	result, err := orch.Execute(context.Background(), ExecutionRequest{
		Message:        "inspect",
		RequestedAgent: "coder",
		SessionID:      "session-42",
		TaskID:         "task-42",
		PolicyContext:  policy,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.SessionID != "session-42" || result.TaskID != "task-42" || result.AgentID != "coder" {
		t.Fatalf("Execute() result = %#v, want request identity", result)
	}
	executionCtx := provider.executionContext(0)
	if got := TaskIDFromContext(executionCtx); got != "task-42" {
		t.Fatalf("TaskIDFromContext() = %q, want task-42", got)
	}
	if got := storage.SessionFromContext(executionCtx); got != "session-42" {
		t.Fatalf("SessionFromContext() = %q, want session-42", got)
	}
	if got := ExecutionPolicyContext(executionCtx)["approval"]; got != "required" {
		t.Fatalf("ExecutionPolicyContext() = %#v, want approval policy", ExecutionPolicyContext(executionCtx))
	}
	if got := TaskIDFromContext(hook.context); got != "task-42" {
		t.Fatalf("hook task ID = %q, want task-42", got)
	}
}

func TestExecuteRequestedAgentAndChatAdaptersRemainCompatible(t *testing.T) {
	coder := &executionTestProvider{name: "coder", modelID: "test"}
	reviewer := &executionTestProvider{name: "reviewer", modelID: "test"}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{
			"coder":    newExecutionTestAgent("coder", coder),
			"reviewer": newExecutionTestAgent("reviewer", reviewer),
		},
		active: "coder",
	}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "review", RequestedAgent: "reviewer"})
	if err != nil || result.AgentID != "reviewer" || result.Response.Content != "reviewer" {
		t.Fatalf("explicit Execute() = %#v, %v", result, err)
	}
	if response, err := orch.Chat(context.Background(), "chat"); err != nil || response.Content != "coder" {
		t.Fatalf("Chat() = %#v, %v", response, err)
	}
	stream, err := orch.ChatStream(context.Background(), "stream")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}
}

func TestExecuteAppliesPPDPolicyToAutomaticRouting(t *testing.T) {
	ppdConfig := router.PPDConfig{
		Version: "v1", Mode: router.PPDModeEnabled, Specialist: "ppd-planner", MaxPlannerCalls: 1,
		Thresholds: router.PPDThresholds{MinFiles: 3, MinPackages: 2, MinEstimatedCalls: 5},
	}

	t.Run("simple bypass", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "update main.go"})
		if err != nil || result.AgentID != "coder" || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionBypass {
			t.Fatalf("Execute() = %#v, %v; want coder bypass", result, err)
		}
		if len(planner.requests) != 0 {
			t.Fatalf("planner calls = %d, want 0", len(planner.requests))
		}
	})

	t.Run("shadow observation", func(t *testing.T) {
		config := ppdConfig
		config.Mode = router.PPDModeShadow
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(config, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "refactor across multiple packages"})
		if err != nil || result.AgentID != "coder" || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionShadow || result.PPDDecision.Features.Kind != router.TaskKindRefactor || !result.PPDDecision.Features.HighRisk {
			t.Fatalf("Execute() = %#v, %v; want observed shadow on coder", result, err)
		}
		if len(planner.requests) != 0 {
			t.Fatalf("planner calls = %d, want 0", len(planner.requests))
		}
	})

	t.Run("enabled delegation", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "change behavior", PPD: &router.PPDRequest{FileCount: 3}})
		if err != nil || result.AgentID != "ppd-planner" || result.Response.Content != "planner" || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionDelegate {
			t.Fatalf("Execute() = %#v, %v; want planner delegation", result, err)
		}
	})

	t.Run("explicit agent bypass", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "change behavior", RequestedAgent: "coder", PPD: &router.PPDRequest{ExplicitPPD: true}})
		if err != nil || result.AgentID != "coder" || result.PPDDecision != nil {
			t.Fatalf("Execute() = %#v, %v; want explicit coder without PPD decision", result, err)
		}
		if len(planner.requests) != 0 {
			t.Fatalf("planner calls = %d, want 0", len(planner.requests))
		}
	})

	t.Run("missing specialist failure", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, nil)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "change behavior", PPD: &router.PPDRequest{PackageCount: 2}})
		if err == nil || !contains(err.Error(), `PPD specialist "ppd-planner" not found`) || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionDelegate {
			t.Fatalf("Execute() = %#v, %v; want explicit missing specialist failure", result, err)
		}
		if len(coder.requests) != 0 {
			t.Fatalf("coder calls = %d, want 0", len(coder.requests))
		}
	})

	t.Run("call limit bypass", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "change behavior", PPD: &router.PPDRequest{ExplicitPPD: true, PlannerCalls: 1}})
		if err != nil || result.AgentID != "coder" || result.PPDDecision == nil || result.PPDDecision.Reason != "planner_call_limit" {
			t.Fatalf("Execute() = %#v, %v; want planner call-limit bypass", result, err)
		}
		if len(planner.requests) != 0 {
			t.Fatalf("planner calls = %d, want 0", len(planner.requests))
		}
	})
}

func newPPDExecutionTestOrchestrator(config router.PPDConfig, coder, planner *executionTestProvider) *Orchestrator {
	agents := map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", coder)}
	if planner != nil {
		agents["ppd-planner"] = newExecutionTestAgent("ppd-planner", planner)
	}
	return &Orchestrator{
		agents: agents, active: "coder",
		routingConfig: &router.Config{PPD: config},
		routingState:  make(map[string]router.Classification), modelOverrides: make(map[string]bool),
	}
}

func TestExecuteRejectsUnsupportedVerifiedCompletion(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)},
		active: "coder",
	}
	obligations := verification.Derive(verification.Input{
		TaskKind:     "debug",
		ChangedPaths: []string{"main.go"},
		TestCommands: []string{"go test ./..."},
	})

	_, err := orch.Execute(context.Background(), ExecutionRequest{
		Message:                 "fix the bug",
		VerificationMode:        verification.ModeEnforce,
		VerificationObligations: obligations,
		VerificationEvents: []execution.Event{{
			ID: "write", TaskID: "task", Sequence: 1, Type: execution.EventWrite, Paths: []string{"main.go"},
		}},
	})
	if err == nil || !contains(err.Error(), "verification does not support") {
		t.Fatalf("Execute() error = %v, want unsupported verification error", err)
	}

	result, err := orch.Execute(context.Background(), ExecutionRequest{
		Message:                 "fix the bug",
		Mode:                    ExecutionStreaming,
		VerificationMode:        verification.ModeEnforce,
		VerificationObligations: obligations,
		VerificationEvents: []execution.Event{{
			ID: "write", TaskID: "task", Sequence: 1, Type: execution.EventWrite, Paths: []string{"main.go"},
		}},
	})
	if err != nil {
		t.Fatalf("streaming Execute() error = %v", err)
	}
	var streamErr error
	for response := range result.Stream {
		if response.Err != nil {
			streamErr = response.Err
		}
	}
	if streamErr == nil || !contains(streamErr.Error(), "verification does not support") {
		t.Fatalf("streaming verification error = %v, want unsupported verification error", streamErr)
	}
}

func contains(text, substring string) bool {
	for i := 0; i+len(substring) <= len(text); i++ {
		if text[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}

// failNProvider fails the first N calls with the given error, then succeeds.
type failNProvider struct {
	mu        sync.Mutex
	failsLeft int
	failErr   error
}

func (p *failNProvider) Chat(_ context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failsLeft > 0 {
		p.failsLeft--
		return nil, p.failErr
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "recovered"}, nil
}

func (p *failNProvider) StreamChat(_ context.Context, _ *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failsLeft > 0 {
		p.failsLeft--
		return nil, p.failErr
	}
	ch := make(chan *model.ChatResponse, 1)
	ch <- &model.ChatResponse{Role: model.RoleAssistant, Content: "recovered"}
	close(ch)
	return ch, nil
}

func (p *failNProvider) Name() string  { return "test" }
func (p *failNProvider) Model() string { return "test-model" }

func TestExecuteRetriesTransientErrors(t *testing.T) {
	apiErr := &model.APIError{StatusCode: 429, Status: "429 Too Many Requests"}
	provider := &failNProvider{failsLeft: 1, failErr: fmt.Errorf("anthropic chat: %w", apiErr)}
	a := newExecutionTestAgent("coder", provider)
	orch := &Orchestrator{
		agents:         map[string]*agent.Agent{"coder": a},
		active:         "coder",
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
	}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("Execute() should have retried and succeeded, got error: %v", err)
	}
	if result.Response == nil || result.Response.Content != "recovered" {
		t.Fatalf("Execute() response = %v, want recovered", result.Response)
	}
}

// Note: streaming model errors flow through the channel (resp.Err), not as
// direct return values from ChatStream. The agent SDK only returns errors
// directly for pre-flight failures (no model, guardrails, session setup).
// Streaming model error recovery is handled at the TUI layer.

func TestExecuteReturnsClassifiedErrorWhenRetriesExhausted(t *testing.T) {
	apiErr := &model.APIError{StatusCode: 529, Status: "529 Overloaded"}
	provider := &failNProvider{failsLeft: 100, failErr: fmt.Errorf("anthropic chat: %w", apiErr)}
	a := newExecutionTestAgent("coder", provider)
	orch := &Orchestrator{
		agents:         map[string]*agent.Agent{"coder": a},
		active:         "coder",
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
	}

	_, err := orch.Execute(context.Background(), ExecutionRequest{Message: "hello"})
	if err == nil {
		t.Fatal("Execute() should have returned an error after exhausting retries")
	}
	var classified *apierror.Classified
	if !errors.As(err, &classified) {
		t.Fatalf("Execute() error should be *apierror.Classified, got %T: %v", err, err)
	}
	if classified.Category != apierror.CategoryOverloaded {
		t.Errorf("classified.Category = %v, want %v", classified.Category, apierror.CategoryOverloaded)
	}
}

func TestExecuteDoesNotRetryTerminalErrors(t *testing.T) {
	apiErr := &model.APIError{StatusCode: 401, Status: "401 Unauthorized"}
	callCount := 0
	provider := &failNProvider{failsLeft: 100, failErr: fmt.Errorf("anthropic chat: %w", apiErr)}
	// Wrap to count calls
	countingProvider := &countingProviderWrapper{inner: provider, count: &callCount}
	a := newExecutionTestAgent("coder", countingProvider)
	orch := &Orchestrator{
		agents:         map[string]*agent.Agent{"coder": a},
		active:         "coder",
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
	}

	_, err := orch.Execute(context.Background(), ExecutionRequest{Message: "hello"})
	if err == nil {
		t.Fatal("Execute() should have returned an auth error")
	}
	var classified *apierror.Classified
	if !errors.As(err, &classified) {
		t.Fatalf("Execute() error should be *apierror.Classified, got %T", err)
	}
	if classified.Category != apierror.CategoryAuth {
		t.Errorf("classified.Category = %v, want %v", classified.Category, apierror.CategoryAuth)
	}
	if callCount != 1 {
		t.Errorf("terminal errors should not be retried, got %d calls", callCount)
	}
}

type countingProviderWrapper struct {
	inner model.Provider
	count *int
}

func (w *countingProviderWrapper) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	*w.count++
	return w.inner.Chat(ctx, req)
}

func (w *countingProviderWrapper) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	*w.count++
	return w.inner.StreamChat(ctx, req)
}

func (w *countingProviderWrapper) Name() string  { return w.inner.Name() }
func (w *countingProviderWrapper) Model() string { return w.inner.Model() }
