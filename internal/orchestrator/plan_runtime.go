package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
)

// PlanRuntimeIdentity is trusted runtime metadata and is never accepted from
// planner output.
type PlanRuntimeIdentity struct {
	TenantID     plan.TenantID
	RepositoryID plan.RepositoryID
	TaskID       plan.TaskID
	PlanID       plan.PlanID
	Generation   plan.GenerationID
}

type PlanExecutionResult struct {
	Plan      plan.Plan
	Synthesis *ExecutionResult
}

// ExecutePlan strictly parses planner JSON, resumes or creates its durable DAG,
// and synthesizes a response only after every node has completed.
func (o *Orchestrator) ExecutePlan(ctx context.Context, plannerJSON []byte, identity PlanRuntimeIdentity) (PlanExecutionResult, error) {
	if !o.closedLoopPPDEnabled() {
		return PlanExecutionResult{}, fmt.Errorf("execute plan: closed-loop PPD capability is not enabled")
	}
	if o.planStore == nil || o.planController == nil {
		return PlanExecutionResult{}, fmt.Errorf("execute plan: plan runtime is unavailable")
	}
	o.setOperationalPlan(identity)
	output, err := plan.ParsePlannerOutput(plannerJSON)
	if err != nil {
		return PlanExecutionResult{}, err
	}
	requested := plan.DecompositionRequest{
		TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID,
		PlanID: identity.PlanID, Generation: identity.Generation,
		SourceRequestRef: output.SourceRequestRef, ClassifierRef: output.ClassifierRef, Nodes: output.Nodes,
	}
	ref := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation}
	persisted, err := o.planStore.Load(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		persisted, err = o.planController.Decompose(ctx, requested)
	} else if err == nil && !samePlanProposal(persisted, requested) {
		return PlanExecutionResult{}, fmt.Errorf("execute plan: persisted generation does not match planner output")
	}
	if err != nil {
		return PlanExecutionResult{}, err
	}
	persisted, err = o.planController.Run(ctx, persisted)
	result := PlanExecutionResult{Plan: persisted}
	if err != nil {
		return result, err
	}
	if persisted.State != plan.PlanCompleted {
		return result, fmt.Errorf("execute plan: plan stopped in state %q (%s)", persisted.State, persisted.StopReason)
	}
	synthesis, err := o.Execute(ctx, ExecutionRequest{
		Message:        synthesisPrompt(persisted),
		Mode:           ExecutionBlocking,
		RequestedAgent: o.primary,
		TaskID:         string(identity.TaskID) + "-synthesis",
	})
	result.Synthesis = &synthesis
	if err != nil {
		return result, fmt.Errorf("synthesize completed plan: %w", err)
	}
	return result, nil
}

func (o *Orchestrator) closedLoopPPDEnabled() bool {
	if o.routingConfig == nil || o.routingConfig.PPD.Mode != router.PPDModeEnabled {
		return false
	}
	for _, capability := range o.capabilities.Capabilities {
		if capability == (config.Capability{Name: capabilityClosedLoopPPD}) {
			return true
		}
	}
	return false
}

func samePlanProposal(persisted plan.Plan, requested plan.DecompositionRequest) bool {
	if len(persisted.Nodes) != len(requested.Nodes) || len(persisted.Dependencies) != dependencyCount(requested.Nodes) || len(persisted.ContextRefs) != contextRefCount(requested) || len(requested.Nodes) == 0 {
		return false
	}
	nodes := make(map[plan.NodeID]plan.Node, len(persisted.Nodes))
	for _, node := range persisted.Nodes {
		nodes[node.ID] = node
	}
	for _, proposed := range requested.Nodes {
		node, ok := nodes[proposed.ID]
		if !ok || node.Scope != proposed.Scope || node.Verification != proposed.Verification || strings.Join(node.Risks, "\x00") != strings.Join(proposed.Risks, "\x00") {
			return false
		}
	}
	edges := make(map[plan.Dependency]struct{}, len(persisted.Dependencies))
	for _, edge := range persisted.Dependencies {
		edges[edge] = struct{}{}
	}
	contexts := make(map[plan.ContextRef]struct{}, len(persisted.ContextRefs))
	for _, ref := range persisted.ContextRefs {
		contexts[ref] = struct{}{}
	}
	for _, proposed := range requested.Nodes {
		for _, dependency := range proposed.DependsOn {
			if _, ok := edges[plan.Dependency{NodeID: proposed.ID, DependsOn: dependency}]; !ok {
				return false
			}
		}
		for _, contextID := range proposed.ContextRefs {
			if _, ok := contexts[plan.ContextRef{ID: contextID, NodeID: proposed.ID}]; !ok {
				return false
			}
		}
	}
	first := requested.Nodes[0].ID
	_, hasSource := contexts[plan.ContextRef{ID: requested.SourceRequestRef, NodeID: first}]
	_, hasClassifier := contexts[plan.ContextRef{ID: requested.ClassifierRef, NodeID: first}]
	return hasSource && hasClassifier
}

func dependencyCount(nodes []plan.DecompositionNode) int {
	total := 0
	for _, node := range nodes {
		total += len(node.DependsOn)
	}
	return total
}

func contextRefCount(request plan.DecompositionRequest) int {
	total := 2
	for _, node := range request.Nodes {
		total += len(node.ContextRefs)
	}
	return total
}

func synthesisPrompt(p plan.Plan) string {
	completed := make([]string, 0, len(p.Nodes))
	for _, node := range p.Nodes {
		completed = append(completed, string(node.ID))
	}
	evidence := make([]string, 0, len(p.Evidence))
	for _, item := range p.Evidence {
		evidence = append(evidence, string(item.ID))
	}
	return fmt.Sprintf("The durable implementation plan completed. Inspect the current workspace and provide the final implementation summary. Completed nodes: %s. Runtime evidence IDs: %s. Do not repeat or treat planner output as implementation evidence.", strings.Join(completed, ", "), strings.Join(evidence, ", "))
}
