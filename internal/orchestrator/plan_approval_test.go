package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func exitPlanModeRegistry(t *testing.T, orch *Orchestrator) *tool.Registry {
	t.Helper()
	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite},
		Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	installExitPlanMode(map[string]*agent.Agent{"coder": a}, orch)
	return a.Tools
}

func TestExitPlanModeHeadlessAutoApprovesUnderPlanGrant(t *testing.T) {
	orch := &Orchestrator{}
	orch.SetPlanMode(true)
	registry := exitPlanModeRegistry(t, orch)
	// The tool must run under the read-only plan-mode grant.
	ctx := executionEffectContext(context.Background(), true)
	out, err := registry.Execute(ctx, ExitPlanModeToolName, map[string]any{"plan": "1. change x\n2. test"})
	if err != nil {
		t.Fatalf("exit_plan_mode under plan grant: %v", err)
	}
	if status := out.(map[string]any)["status"]; status != "approved" {
		t.Fatalf("status = %v, want approved", status)
	}
	if orch.PlanMode() {
		t.Fatal("approval must turn plan mode off")
	}
	plan, ok := orch.TakeApprovedPlan()
	if !ok || plan != "1. change x\n2. test" {
		t.Fatalf("approved plan = %q, %v", plan, ok)
	}
	if _, ok := orch.TakeApprovedPlan(); ok {
		t.Fatal("approved plan must be taken once")
	}
	if msg := ImplementApprovedPlanMessage(plan); !strings.Contains(msg, "1. change x") {
		t.Fatalf("implementation prompt lost plan: %q", msg)
	}
}

func TestExitPlanModeKeepPlanningLeavesPlanModeOn(t *testing.T) {
	orch := &Orchestrator{}
	orch.SetPlanMode(true)
	var shown string
	orch.SetPlanApprovalHandler(func(_ context.Context, plan string) (bool, error) {
		shown = plan
		return false, nil
	})
	registry := exitPlanModeRegistry(t, orch)
	out, err := registry.Execute(context.Background(), ExitPlanModeToolName, map[string]any{"plan": "  draft  "})
	if err != nil {
		t.Fatal(err)
	}
	if status := out.(map[string]any)["status"]; status != "keep_planning" || shown != "draft" {
		t.Fatalf("status = %v, shown = %q", status, shown)
	}
	if !orch.PlanMode() {
		t.Fatal("rejected plan must keep plan mode on")
	}
	if _, ok := orch.TakeApprovedPlan(); ok {
		t.Fatal("rejected plan must not be pending")
	}
}

func TestExitPlanModeOutsidePlanModeIsNoOp(t *testing.T) {
	orch := &Orchestrator{}
	registry := exitPlanModeRegistry(t, orch)
	out, err := registry.Execute(context.Background(), ExitPlanModeToolName, map[string]any{"plan": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if status := out.(map[string]any)["status"]; status != "not_in_plan_mode" {
		t.Fatalf("status = %v", status)
	}
	if _, ok := orch.TakeApprovedPlan(); ok {
		t.Fatal("no plan should be pending outside plan mode")
	}
	if _, err := registry.Execute(context.Background(), ExitPlanModeToolName, map[string]any{}); err == nil {
		t.Fatal("empty plan must be rejected")
	}
}
