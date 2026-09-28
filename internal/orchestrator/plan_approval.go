package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

// ExitPlanModeToolName is the tool the model calls in plan mode to present a
// finished plan for approval.
const ExitPlanModeToolName = "exit_plan_mode"

const exitPlanModeDescription = `Present your finished implementation plan for user approval while PLAN MODE is on.
Call this once the plan is complete: pass the full plan as markdown (numbered steps, files to change, verification).
If approved, plan mode turns off and implementation starts automatically in the next turn.
Do not call this for pure questions or when plan mode is off.`

// PlanApprovalFunc asks a human to approve a plan. It blocks until they decide
// and returns true to approve. A nil handler (headless runs) auto-approves.
type PlanApprovalFunc func(ctx context.Context, plan string) (bool, error)

// SetPlanApprovalHandler installs the interactive plan approval prompt.
func (o *Orchestrator) SetPlanApprovalHandler(handler PlanApprovalFunc) {
	if o == nil {
		return
	}
	o.planApprovalMu.Lock()
	o.planApproval = handler
	o.planApprovalMu.Unlock()
}

func (o *Orchestrator) planApprovalHandler() PlanApprovalFunc {
	o.planApprovalMu.Lock()
	defer o.planApprovalMu.Unlock()
	return o.planApproval
}

// TakeApprovedPlan returns and clears the plan approved during the last
// plan-mode turn. Callers start the implementation turn with it: the effect
// grant of the planning turn is fixed at turn start, so edits need a new turn.
func (o *Orchestrator) TakeApprovedPlan() (string, bool) {
	if o == nil {
		return "", false
	}
	o.planApprovalMu.Lock()
	defer o.planApprovalMu.Unlock()
	plan, ok := o.approvedPlan, o.hasApprovedPlan
	o.approvedPlan, o.hasApprovedPlan = "", false
	return plan, ok
}

func (o *Orchestrator) approvePlan(plan string) {
	o.planApprovalMu.Lock()
	o.approvedPlan, o.hasApprovedPlan = plan, true
	o.planApprovalMu.Unlock()
	o.SetPlanMode(false)
}

// ImplementApprovedPlanMessage is the prompt for the turn that follows an
// approved plan.
func ImplementApprovedPlanMessage(plan string) string {
	return "The user approved the plan below and plan mode is now off. Implement it now, end to end, " +
		"tracking progress with update_plan and verifying the result.\n\nApproved plan:\n" + plan
}

func installExitPlanMode(agents map[string]*agent.Agent, o *Orchestrator) {
	for _, a := range agents {
		if a == nil || a.Tools == nil {
			continue
		}
		// Only agents that can implement a plan can ask to leave plan mode.
		if _, writes := a.Tools.Get("file_write"); !writes {
			continue
		}
		a.Tools.Register(newExitPlanModeTool(o))
	}
}

func newExitPlanModeTool(o *Orchestrator) *tool.Definition {
	return &tool.Definition{
		Name:        ExitPlanModeToolName,
		Description: exitPlanModeDescription,
		Permission:  tool.PermAllow,
		// Session state only; allowed under the plan-mode grant.
		Effects: []tool.Effect{tool.EffectScratchWrite},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan": map[string]any{
					"type":        "string",
					"description": "The complete implementation plan in markdown.",
				},
			},
			"required": []any{"plan"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			plan, _ := args["plan"].(string)
			plan = strings.TrimSpace(plan)
			if plan == "" {
				return nil, fmt.Errorf("%s: 'plan' is required", ExitPlanModeToolName)
			}
			if !o.PlanMode() {
				return map[string]any{"status": "not_in_plan_mode", "message": "Plan mode is off; continue with the task."}, nil
			}
			approved := true
			if handler := o.planApprovalHandler(); handler != nil {
				var err error
				if approved, err = handler(ctx, plan); err != nil {
					return nil, fmt.Errorf("%s: %w", ExitPlanModeToolName, err)
				}
			}
			if !approved {
				return map[string]any{"status": "keep_planning",
					"message": "The user wants to keep planning. Stop now and wait for their feedback; do not call exit_plan_mode again this turn."}, nil
			}
			o.approvePlan(plan)
			return map[string]any{"status": "approved",
				"message": "The user approved the plan and plan mode is off. End this turn now with a one-line confirmation; implementation starts automatically in the next turn with edit permissions."}, nil
		},
	}
}
