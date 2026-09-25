package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"
	storagesqlite "github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/activation"
	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/workspace"
)

type executionTestProvider struct {
	name    string
	modelID string
	usage   model.Usage
	known   bool

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
	return &model.ChatResponse{Role: model.RoleAssistant, Content: p.name, Usage: p.usage, UsageKnown: p.known}, nil
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

func TestExecuteAttachesAndPreservesRequestWorkspaceRoot(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test-model"}
	configuredRoot := t.TempDir()
	requestRoot := t.TempDir()
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)},
		active: "coder", workspace: &workspace.Info{Root: configuredRoot},
	}
	if _, err := orch.Execute(builtins.WithWorkspaceRoot(context.Background(), requestRoot), ExecutionRequest{Message: "inspect", RequestedAgent: "coder"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := builtins.WorkspaceRootFromContext(provider.executionContext(0)); !ok || got != requestRoot {
		t.Fatalf("provider workspace root = %q, %v; want %q", got, ok, requestRoot)
	}
}

func TestExecuteAttachesHostRunIdentity(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test-model"}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)},
		active: "coder", workspace: &workspace.Info{Root: t.TempDir()},
	}
	ctx := authorization.WithRequest(context.Background(), authorization.Request{PrincipalID: "user-1", TenantID: "tenant-1", RepositoryID: "repo-1", Action: "chat.execute"})
	result, err := orch.Execute(ctx, ExecutionRequest{
		Message: "inspect", RequestedAgent: "coder", SessionID: "session-1", TaskID: "task-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := agent.RunIdentityFromContext(provider.executionContext(0))
	if !ok || identity.TenantID != "tenant-1" || identity.RepositoryID != "repo-1" || identity.TaskID != result.TaskID || identity.SessionID != result.SessionID || identity.RoleID != result.AgentID || identity.InvocationID == "" || identity.ParentInvocationID != "" {
		t.Fatalf("run identity = %+v, ok=%v, result=%+v", identity, ok, result)
	}
}

func TestExecutePreservesTrustedParentDeliveryAndNodeIdentity(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test-model"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)}, active: "coder", workspace: &workspace.Info{Root: t.TempDir()}}
	parent := agent.RunIdentity{
		TenantID: "tenant", RepositoryID: "repo", DeliveryID: "delivery", TaskID: "delivery", NodeID: "node", AttemptID: "attempt",
		GoalRevision: "goal", ArtifactSnapshot: "artifact", PolicyRevision: "policy", RoleID: "worker", InvocationID: "parent-run",
	}
	ctx := agent.WithRunIdentity(context.Background(), parent)
	if _, err := orch.Execute(ctx, ExecutionRequest{Message: "inspect", RequestedAgent: "coder", TaskID: "node-task"}); err != nil {
		t.Fatal(err)
	}
	child, ok := agent.RunIdentityFromContext(provider.executionContext(0))
	if !ok || child.TenantID != parent.TenantID || child.RepositoryID != parent.RepositoryID || child.DeliveryID != parent.DeliveryID || child.NodeID != parent.NodeID || child.AttemptID != parent.AttemptID || child.GoalRevision != parent.GoalRevision || child.ArtifactSnapshot != parent.ArtifactSnapshot || child.PolicyRevision != parent.PolicyRevision || child.TaskID != "node-task" || child.RoleID != "coder" || child.ParentInvocationID != parent.InvocationID || child.InvocationID == parent.InvocationID {
		t.Fatalf("child identity = %+v, ok=%v", child, ok)
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
	if got := orch.agents["coder"].Model; got.Name() != "coder" || got.Model() != "coder-model" {
		t.Fatalf("primary model = (%q, %q), want unchanged configured model", got.Name(), got.Model())
	}
	req := routed.request(0)
	if req == nil {
		t.Fatal("expected a model request on the routed primary provider")
	}
	selected, ok := agent.ModelProviderFromContext(routed.executionContext(0))
	if !ok || selected != routed {
		t.Fatalf("provider context = (%v, %v), want routed provider", selected, ok)
	}
	joined := ""
	for _, msg := range req.Messages {
		joined += msg.Content
	}
	if !strings.Contains(joined, "spawn_subagent debugger") || !strings.Contains(joined, "Path: complexity=") {
		t.Fatalf("prompt missing specialist or path hint: %q", joined)
	}
}

type routingRaceProvider struct {
	name    string
	started chan string
	release <-chan struct{}
}

func (p *routingRaceProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.started <- req.Model
	select {
	case <-p.release:
		return &model.ChatResponse{Role: model.RoleAssistant, Content: p.name}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *routingRaceProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("unexpected stream call")
}

func (p *routingRaceProvider) Name() string  { return p.name }
func (p *routingRaceProvider) Model() string { return p.name + "-model" }

func TestExecuteUsesConcurrentRequestScopedProviders(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	low := &routingRaceProvider{name: "low", started: started, release: release}
	high := &routingRaceProvider{name: "high", started: started, release: release}
	configured := &executionTestProvider{name: "configured", modelID: "configured-model"}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", configured)},
		active: "coder",
		routingConfig: &router.Config{ModelRouting: router.ModelRouting{Models: map[router.Complexity]map[router.TaskKind]router.ModelSpec{
			router.ComplexityLow:  {router.TaskKindExplain: {Provider: "low", Model: "low-model"}},
			router.ComplexityHigh: {router.TaskKindExplain: {Provider: "high", Model: "high-model"}},
		}}},
		modelOverrides: make(map[string]bool),
	}
	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		if cfg.Provider == "high" {
			return high, nil
		}
		return low, nil
	}

	type outcome struct {
		result ExecutionResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, message := range []string{"explain this", "explain all files"} {
		message := message
		go func() {
			result, err := orch.Execute(context.Background(), ExecutionRequest{Message: message, RequestedAgent: "coder"})
			outcomes <- outcome{result: result, err: err}
		}()
	}

	seen := map[string]bool{}
	for range 2 {
		select {
		case modelID := <-started:
			seen[modelID] = true
		case <-time.After(time.Second):
			t.Fatal("concurrent provider calls did not overlap")
		}
	}
	close(release)
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
	}
	if !seen["low-model"] || !seen["high-model"] {
		t.Fatalf("request models = %v, want low-model and high-model", seen)
	}
	if orch.agents["coder"].Model != configured {
		t.Fatal("automatic routing mutated Agent.Model")
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
		Version: "v1", Mode: router.PPDModeEnabled, Specialist: "delivery-strategist", MaxPlannerCalls: 1,
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

	t.Run("high complexity delegates", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "refactor across multiple packages"})
		if err != nil || result.AgentID != "delivery-strategist" || result.Response.Content != "planner" || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionDelegate || result.PPDDecision.Reason != "high_risk" {
			t.Fatalf("Execute() = %#v, %v; want live PPD delegation on high complexity", result, err)
		}
		if len(coder.requests) != 0 {
			t.Fatalf("coder calls = %d, want 0", len(coder.requests))
		}
	})

	t.Run("enabled delegation", func(t *testing.T) {
		coder := &executionTestProvider{name: "coder", modelID: "test"}
		planner := &executionTestProvider{name: "planner", modelID: "test"}
		orch := newPPDExecutionTestOrchestrator(ppdConfig, coder, planner)

		result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "change behavior", PPD: &router.PPDRequest{FileCount: 3}})
		if err != nil || result.AgentID != "delivery-strategist" || result.Response.Content != "planner" || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionDelegate {
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
		if err == nil || !contains(err.Error(), `delivery strategist "delivery-strategist" not found`) || result.PPDDecision == nil || result.PPDDecision.Action != router.PPDActionDelegate {
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
		agents["delivery-strategist"] = newExecutionTestAgent("delivery-strategist", planner)
	}
	return &Orchestrator{
		agents: agents, active: "coder",
		routingConfig:  &router.Config{PPD: config},
		modelOverrides: make(map[string]bool),
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

	blocking, err := orch.Execute(context.Background(), ExecutionRequest{
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
	if blocking.Verification.Allowed || !blocking.Verification.Disagreement {
		t.Fatalf("blocking verification = %#v, want visible disagreement", blocking.Verification)
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
	completion, ok := <-result.Completion
	if !ok || completion.Verification.Allowed || !completion.Verification.Disagreement {
		t.Fatalf("streaming verification = %#v, open=%v", completion, ok)
	}
}

func TestExecuteReportModeExposesVerificationDisagreement(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)}, active: "coder"}
	obligations := []verification.Obligation{{
		ID: "test", Kind: verification.KindTest, CommandClass: execution.CommandTest, Paths: []string{"main.go"}, Status: verification.StatusPending,
	}}
	result, err := orch.Execute(context.Background(), ExecutionRequest{
		Message: "fix the bug", VerificationMode: verification.ModeReport, VerificationObligations: obligations,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Verification.Allowed || !result.Verification.Disagreement || len(result.Verification.Obligations) != 1 {
		t.Fatalf("report verification = %#v", result.Verification)
	}
	if result.StopReason != execution.StopSuccess {
		t.Fatalf("report stop reason = %q, want success", result.StopReason)
	}

	streaming, err := orch.Execute(context.Background(), ExecutionRequest{
		Message: "fix the bug", Mode: ExecutionStreaming, VerificationMode: verification.ModeReport, VerificationObligations: obligations,
	})
	if err != nil {
		t.Fatalf("streaming Execute() error = %v", err)
	}
	for response := range streaming.Stream {
		if response.Err != nil {
			t.Fatalf("streaming response error = %v", response.Err)
		}
	}
	completion, ok := <-streaming.Completion
	if !ok || completion.Err != nil || completion.StopReason != execution.StopSuccess || !completion.Verification.Allowed || !completion.Verification.Disagreement {
		t.Fatalf("streaming report completion = %#v, open=%v", completion, ok)
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
	calls     atomic.Int32
	started   chan struct{}
}

func (p *failNProvider) Chat(_ context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls.Add(1)
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failsLeft > 0 {
		p.failsLeft--
		return nil, p.failErr
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "recovered"}, nil
}

func (p *failNProvider) StreamChat(_ context.Context, _ *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	p.calls.Add(1)
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
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

func TestExecutePreservesSDKTransientRetry(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%d/persistent=%t", mode, persistent), func(t *testing.T) {
				provider := &failNProvider{failsLeft: 1, failErr: &model.APIError{StatusCode: 429}}
				a := newExecutionTestAgent("coder", provider)
				if persistent {
					a.Storage = storagememory.New()
				}
				orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}
				t.Cleanup(func() { _ = orch.Close() })
				response, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "hello", SessionID: "retry-session", Mode: mode})
				if err != nil || response == nil || response.Content != "recovered" || provider.calls.Load() != 2 {
					t.Fatalf("response=%+v err=%v calls=%d; want SDK recovery in 2 attempts", response, err, provider.calls.Load())
				}
			})
		}
	}
}

// Classification is presentation metadata, not permission to retry a chat. The
// SDK alone decides which requests can be retried and when its bound is spent.
func TestExecuteReturnsClassifiedErrorWithoutOuterRetry(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		for _, failure := range []struct {
			name     string
			err      error
			category apierror.Category
			calls    int32
		}{
			{"exhausted", &model.APIError{StatusCode: 529, RetryAfter: time.Millisecond}, apierror.CategoryOverloaded, 2},
			{"auth", &model.APIError{StatusCode: 401}, apierror.CategoryAuth, 1},
			{"untyped-overload", errors.New("provider overloaded"), apierror.CategoryOverloaded, 1},
			{"long-retry-after", &model.APIError{StatusCode: 429, RetryAfter: time.Hour}, apierror.CategoryRateLimited, 1},
		} {
			t.Run(fmt.Sprintf("mode=%d/%s", mode, failure.name), func(t *testing.T) {
				provider := &failNProvider{failsLeft: 100, failErr: failure.err}
				orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)}, active: "coder"}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, err := executeTestTurn(ctx, orch, ExecutionRequest{Message: "hello", Mode: mode})
				var classified *apierror.Classified
				if !errors.Is(err, failure.err) || !errors.As(err, &classified) || classified.Category != failure.category || provider.calls.Load() != failure.calls {
					t.Fatalf("error=%v (%T) calls=%d; want category=%s, original error and %d SDK attempts", err, err, provider.calls.Load(), failure.category, failure.calls)
				}
			})
		}
	}
}

func TestExecuteCancellationStopsSDKRetry(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &failNProvider{failsLeft: 100, failErr: &model.APIError{StatusCode: 503, RetryAfter: time.Second}, started: make(chan struct{}, 1)}
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)}, active: "coder"}
			done := make(chan error, 1)
			go func() {
				_, err := executeTestTurn(ctx, orch, ExecutionRequest{Message: "hello", Mode: mode})
				done <- err
			}()
			select {
			case <-provider.started:
			case <-time.After(3 * time.Second):
				t.Fatal("provider call did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || provider.calls.Load() != 1 {
					t.Fatalf("cancellation error=%v calls=%d; want context.Canceled and one attempt", err, provider.calls.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not stop execution")
			}
		})
	}
}

type executionRejectingGuard struct {
	checks atomic.Int32
}

func (g *executionRejectingGuard) Check(context.Context, string) guardrails.Result {
	g.checks.Add(1)
	return guardrails.Result{Reason: "context_length exceeded"}
}

func TestExecutePreflightFailureDoesNotResubmitSession(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			provider := &failNProvider{}
			a := newExecutionTestAgent("coder", provider)
			a.Storage = storagememory.New()
			guard := &executionRejectingGuard{}
			a.Guardrails.AddRule(guardrails.Rule{Name: "preflight", Position: guardrails.Input, Guardrail: guard})
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", sessions: map[string]string{"coder": "preflight-session"}}
			t.Cleanup(func() { _ = orch.Close() })
			// Session chat persists the user before input validation. Even a
			// synchronous streaming-start failure must not resubmit that user.
			_, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "task", SessionID: "preflight-session", Mode: mode})
			var classified *apierror.Classified
			if !errors.As(err, &classified) || classified.Category != apierror.CategoryContextLength {
				t.Fatalf("preflight error=%v (%T), want classified context error", err, err)
			}
			events, err := a.Storage.ListEvents(context.Background(), "preflight-session", 0)
			if err != nil {
				t.Fatal(err)
			}
			if guard.checks.Load() != 1 || provider.calls.Load() != 0 || len(events) != 1 {
				t.Fatalf("preflight checks=%d provider calls=%d ledger events=%d; want 1, 0, 1", guard.checks.Load(), provider.calls.Load(), len(events))
			}
		})
	}
}

type mutationFailureProvider struct {
	calls atomic.Int32
	err   error
}

func (p *mutationFailureProvider) Name() string  { return "mutation-failure" }
func (p *mutationFailureProvider) Model() string { return "test-model" }
func (p *mutationFailureProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls.Add(1)
	for _, message := range req.Messages {
		if message.Role == model.RoleTool {
			return nil, p.err
		}
	}
	return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{
		ID: "mutation", Name: "mutate", Arguments: `{}`,
	}}}, nil
}

func (p *mutationFailureProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	response, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	stream := make(chan *model.ChatResponse, 1)
	stream <- response
	close(stream)
	return stream, nil
}

// executeTestTurn drains the public stream so ledger/episode assertions observe
// completed execution, including the SDK's terminal error chunk.
func executeTestTurn(ctx context.Context, orch *Orchestrator, request ExecutionRequest) (*model.ChatResponse, error) {
	result, err := orch.Execute(ctx, request)
	if err != nil || result.Stream == nil {
		return result.Response, err
	}
	response := &model.ChatResponse{}
	for chunk := range result.Stream {
		if chunk != nil {
			response.Content += chunk.Content
			if chunk.StopReason != "" {
				response.StopReason = chunk.StopReason
			}
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	return response, err
}

func TestExecuteFailureAfterMutationDoesNotReplayTask(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		for _, failure := range []struct {
			name     string
			err      *model.APIError
			category apierror.Category
			calls    int32
		}{
			{"transient", &model.APIError{StatusCode: 503, RetryAfter: time.Millisecond}, apierror.CategoryServerError, 3},
			{"too-large", &model.APIError{StatusCode: 413}, apierror.CategoryRequestTooLarge, 2},
			{"context-length", &model.APIError{StatusCode: 400, Body: "context_length exceeded"}, apierror.CategoryContextLength, 2},
		} {
			t.Run(fmt.Sprintf("mode=%d/%s", mode, failure.name), func(t *testing.T) {
				ctx := storage.WithSession(context.Background(), "mutation-session")
				store, err := storagesqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
				if err != nil {
					t.Fatal(err)
				}
				orch := &Orchestrator{store: store}
				t.Cleanup(func() { _ = orch.Close() })
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				provider := &mutationFailureProvider{err: failure.err}
				a := newExecutionTestAgent("coder", provider)
				a.Storage = store
				var mutations atomic.Int32
				a.Tools.Register(&tool.Definition{Name: "mutate", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectExternalMutation}, Handler: func(context.Context, map[string]any) (any, error) {
					mutations.Add(1)
					return "mutation committed", nil
				}})
				layers, err := memory.OpenLayerStore(ctx, filepath.Join(t.TempDir(), "memory.db"))
				if err != nil {
					t.Fatal(err)
				}
				tracker := budget.NewTracker(1000, 500)
				if err := tracker.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 100}}}); err != nil {
					t.Fatal(err)
				}
				*orch = Orchestrator{
					agents: map[string]*agent.Agent{"coder": a}, active: "coder", store: store,
					sessions: map[string]string{"coder": "mutation-session"}, budget: tracker,
					runtimeMemory: &runtimeMemory{store: layers, options: memory.LayerOptions{ProjectID: "project"}, stop: make(chan struct{})},
				}
				_, executionErr := executeTestTurn(ctx, orch, ExecutionRequest{
					Message: "perform mutation", RequestedAgent: "coder", SessionID: "mutation-session", Mode: mode,
				})
				events, err := store.ListEvents(ctx, "mutation-session", 0)
				if err != nil {
					t.Fatal(err)
				}
				users := 0
				for _, event := range events {
					if payload, ok := event.Payload.(map[string]any); ok && event.Type == "chat_message" && payload["role"] == "user" {
						users++
					}
				}
				if mutations.Load() != 1 || users != 1 || provider.calls.Load() != failure.calls {
					t.Errorf("mutations=%d user ledger entries=%d provider calls=%d; want 1, 1, %d", mutations.Load(), users, provider.calls.Load(), failure.calls)
				}
				if used := tracker.Used("mutation-session"); used != 100 {
					t.Errorf("error reset cumulative usage: got %d, want 100", used)
				}
				var classified *apierror.Classified
				if !errors.Is(executionErr, failure.err) || !errors.As(executionErr, &classified) || classified.Category != failure.category {
					t.Errorf("error=%v (%T), want original error with category %s", executionErr, executionErr, failure.category)
				}
				episodes, err := layers.Recall(ctx, orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject, Kind: memory.KindEpisodic})
				if err != nil || len(episodes) != 1 || !strings.Contains(episodes[0].Content, "Outcome (failed)") || episodes[0].Provenance.SessionID != "mutation-session" {
					t.Errorf("failure episode forwarding: %+v, %v", episodes, err)
				}
			})
		}
	}
}

type outputLimitedProvider struct {
	mu        sync.Mutex
	responses []*model.ChatResponse
	requests  []*model.ChatRequest
	calls     int
}

func (p *outputLimitedProvider) next(req *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("unexpected model call %d", p.calls+1)
	}
	response := *p.responses[p.calls]
	p.calls++
	return &response, nil
}

func (p *outputLimitedProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	return p.next(req)
}

func (p *outputLimitedProvider) StreamChat(_ context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	response, err := p.next(req)
	if err != nil {
		return nil, err
	}
	stream := make(chan *model.ChatResponse, 2)
	if response.Content != "" {
		stream <- &model.ChatResponse{Role: model.RoleAssistant, Delta: true, Content: response.Content}
	}
	stream <- &model.ChatResponse{Role: model.RoleAssistant, StopReason: response.StopReason}
	close(stream)
	return stream, nil
}

func (*outputLimitedProvider) Name() string  { return "test" }
func (*outputLimitedProvider) Model() string { return "test-model" }

func TestExecuteContinuesOutputLimitedResponse(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			provider := &outputLimitedProvider{responses: []*model.ChatResponse{
				{Role: model.RoleAssistant, Content: "partial ", StopReason: model.StopReasonMaxTokens},
				{Role: model.RoleAssistant, Content: "completion", StopReason: model.StopReasonEnd},
			}}
			a := newExecutionTestAgent("coder", provider)
			a.Storage = storagememory.New()
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}
			t.Cleanup(func() { _ = orch.Close() })

			response, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "original task", SessionID: "continuation-session", Mode: mode})
			if err != nil || response == nil || response.Content != "partial completion" {
				t.Fatalf("response=%+v err=%v, want completed response", response, err)
			}
			if provider.calls != 2 {
				t.Fatalf("model calls=%d, want 2", provider.calls)
			}
			last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
			if last.Role != model.RoleUser || last.Content != continuationPrompt {
				t.Fatalf("continuation request last message=%+v, want continuation prompt", last)
			}
		})
	}
}

func TestExecuteStopsAfterMaximumOutputSegments(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			provider := &outputLimitedProvider{responses: []*model.ChatResponse{
				{Role: model.RoleAssistant, Content: "one ", StopReason: model.StopReasonMaxTokens},
				{Role: model.RoleAssistant, Content: "two ", StopReason: model.StopReasonMaxTokens},
				{Role: model.RoleAssistant, Content: "three", StopReason: model.StopReasonMaxTokens},
				{Role: model.RoleAssistant, Content: "must not be requested", StopReason: model.StopReasonEnd},
			}}
			a := newExecutionTestAgent("coder", provider)
			a.Storage = storagememory.New()
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}
			t.Cleanup(func() { _ = orch.Close() })

			response, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "original task", SessionID: "continuation-session", Mode: mode})
			if err != nil || response == nil || response.Content != "one two three" {
				t.Fatalf("response=%+v err=%v, want bounded partial response", response, err)
			}
			if response.StopReason != model.StopReasonMaxTokens {
				t.Fatalf("stop reason=%q, want %q", response.StopReason, model.StopReasonMaxTokens)
			}
			if provider.calls != maxOutputSegments {
				t.Fatalf("model calls=%d, want %d", provider.calls, maxOutputSegments)
			}
		})
	}
}

func TestExecutePrefetchesRepositoryContext(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"go.mod":  "module demo\n\ngo 1.24\n",
		"main.go": "package demo\n\n// BuildAgent assembles an agent.\nfunc BuildAgent() error { return nil }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := graph.NewIndexScope(context.Background(), graph.IndexScopeOptions{Root: root, DataDir: t.TempDir(), IndexOnStart: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	<-scope.Engine().Ready()
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	orch := &Orchestrator{
		agents:     map[string]*agent.Agent{"coder": newExecutionTestAgent("coder", provider)},
		active:     "coder",
		cfg:        &config.Config{},
		graphStore: scope.Live(),
		graphScope: scope,
		actBuf:     activation.NewBuffer(1),
	}
	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "fix BuildAgent"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := userContent(provider.request(0))
	if !contains(prompt, "[Repository context]") || !contains(prompt, "func BuildAgent() error") || contains(prompt, "[Pre-loaded context]") {
		t.Fatalf("prompt = %q", prompt)
	}
	if src := contextSource(result.ContextReport, ContextSourceRepositoryContext); src.SelectedCount == 0 || src.Bytes == 0 {
		t.Fatalf("repository context report = %#v", src)
	}

	off := 0
	orch.cfg = &config.Config{Workspace: config.WorkspaceConfig{Indexer: config.IndexerConfig{PrefetchTokens: &off}}}
	if _, err := orch.Execute(context.Background(), ExecutionRequest{Message: "fix BuildAgent"}); err != nil {
		t.Fatal(err)
	}
	if prompt := userContent(provider.request(1)); contains(prompt, "[Repository context]") {
		t.Fatalf("prefetch_tokens: 0 must disable prefetch: %q", prompt)
	}
}
