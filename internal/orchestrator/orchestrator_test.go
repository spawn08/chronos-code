package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/router"
)

type routingTestProvider struct {
	provider string
	model    string
}

func (p *routingTestProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) Name() string  { return p.provider }
func (p *routingTestProvider) Model() string { return p.model }

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
