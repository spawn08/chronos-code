package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/worktree"
)

// PlanRuntimeIdentity is trusted runtime metadata and is never accepted from
// strategist output.
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

// ExecutePlan strictly parses strategist JSON, resumes or creates its durable DAG,
// and synthesizes a response only after every node has completed.
func (o *Orchestrator) ExecutePlan(ctx context.Context, strategistJSON []byte, identity PlanRuntimeIdentity) (PlanExecutionResult, error) {
	if !o.closedLoopPPDEnabled() {
		return PlanExecutionResult{}, fmt.Errorf("execute plan: closed-loop PPD capability is not enabled")
	}
	if o.planStore == nil || o.planController == nil {
		return PlanExecutionResult{}, fmt.Errorf("execute plan: plan runtime is unavailable")
	}
	o.setOperationalPlan(identity)
	output, err := plan.ParseStrategistOutput(strategistJSON)
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
		return PlanExecutionResult{}, fmt.Errorf("execute plan: persisted generation does not match strategist output")
	}
	if err != nil {
		return PlanExecutionResult{}, err
	}
	if err := o.validatePlanArtifactReceipts(persisted); err != nil {
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

func (o *Orchestrator) validatePlanArtifactReceipts(p plan.Plan) error {
	if o.worktreeManager == nil {
		return nil
	}
	for _, artifact := range p.Artifacts {
		if artifact.ReceiptID == "" {
			continue
		}
		receipt, err := o.worktreeManager.LoadReceipt(artifact.ReceiptID)
		if err != nil {
			return fmt.Errorf("load accepted node %q integration receipt: %w", artifact.NodeID, err)
		}
		if receipt.ArtifactID != artifact.ID || artifact.Undone != (receipt.State == "undone") || receipt.State == "undo_prepared" {
			return fmt.Errorf("plan node %q artifact state requires reconciliation", artifact.NodeID)
		}
	}
	return nil
}

// UndoPlanArtifact is an authorized compensating action for an accepted leaf.
// It never attempts to reverse API/process effects or completed descendants.
// A crash after the file undo but before its plan transition can be retried
// with the same receipt, which remains durable in the worktree manager.
func (o *Orchestrator) UndoPlanArtifact(ctx context.Context, identity PlanRuntimeIdentity, nodeID plan.NodeID) error {
	auth, ok := authorization.FromContext(ctx)
	if !ok || auth.Action != "plan.undo" || auth.TenantID != string(identity.TenantID) || auth.RepositoryID != string(identity.RepositoryID) {
		return authorization.ErrDenied
	}
	if o.planStore == nil || o.worktreeManager == nil || o.workspace == nil {
		return fmt.Errorf("plan artifact undo runtime is unavailable")
	}
	ref := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation}
	loaded, err := o.planStore.Load(ctx, ref)
	if err != nil {
		return err
	}
	var receiptID string
	for _, artifact := range loaded.Artifacts {
		if artifact.NodeID == nodeID {
			receiptID = artifact.ReceiptID
			break
		}
	}
	if receiptID == "" {
		return plan.ErrArtifactUndoBlocked
	}
	version, err := o.planStore.ArtifactUndoEligible(ctx, ref, nodeID, receiptID)
	if err != nil {
		return err
	}
	if err := o.worktreeManager.Undo(ctx, o.workspace.Root, receiptID); err != nil {
		return err
	}
	return o.planStore.RecordArtifactUndo(ctx, ref, nodeID, receiptID, version)
}

// ReconcilePlanReceipt is an operator-only recovery of one verified node whose
// file integration was observed after its plan lease expired. It never repeats
// the patch and never asserts success for unrelated tool/provider effects.
func (o *Orchestrator) ReconcilePlanReceipt(ctx context.Context, deliveries *execution.DeliveryStore, identity PlanRuntimeIdentity, nodeID plan.NodeID, attemptID plan.AttemptID, receiptID string, expectedDeliveryVersion int64) error {
	auth, ok := authorization.FromContext(ctx)
	if !ok || auth.Action != "plan.reconcile" || auth.TenantID != string(identity.TenantID) || auth.RepositoryID != string(identity.RepositoryID) {
		return authorization.ErrDenied
	}
	if deliveries == nil || o == nil || o.planStore == nil || o.worktreeManager == nil || o.workspace == nil || identity.PlanID != plan.PlanID(identity.TaskID) || identity.Generation == "" {
		return execution.ErrInvalidDelivery
	}
	root, err := filepath.EvalSymlinks(o.workspace.Root)
	if err != nil {
		return fmt.Errorf("resolve plan receipt repository: %w", err)
	}
	ref := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation}
	scope := execution.DeliveryScope{TenantID: execution.TenantID(identity.TenantID), RepositoryID: execution.RepositoryID(identity.RepositoryID)}
	return deliveries.WithParkedPlan(ctx, scope, execution.DeliveryID(identity.TaskID), expectedDeliveryVersion, func(guarded context.Context) error {
		version, err := o.planStore.Version(guarded, ref)
		if err != nil {
			return err
		}
		return o.worktreeManager.WithVerifiedPlanReceipt(guarded, root, receiptID, string(identity.TaskID), string(attemptID), func(receipt worktree.IntegrationReceipt) error {
			return o.planStore.ReconcileAppliedReceipt(guarded, ref, nodeID, attemptID, receipt.ArtifactID, receipt.ID, version)
		})
	})
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
		if !ok || node.Kind != proposed.Kind || node.Objective != proposed.Objective || node.Scope != proposed.Scope ||
			node.RecoveryClass != proposed.RecoveryClass || node.Verification != proposed.Verification ||
			strings.Join(node.ExpectedArtifacts, "\x00") != strings.Join(proposed.ExpectedArtifacts, "\x00") ||
			strings.Join(node.Assumptions, "\x00") != strings.Join(proposed.Assumptions, "\x00") ||
			strings.Join(node.InvalidationTriggers, "\x00") != strings.Join(proposed.InvalidationTriggers, "\x00") ||
			strings.Join(node.Risks, "\x00") != strings.Join(proposed.Risks, "\x00") {
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
	return fmt.Sprintf("The durable implementation plan completed. Inspect the current workspace and provide the final implementation summary. Completed nodes: %s. Runtime evidence IDs: %s. Do not repeat or treat strategist output as implementation evidence.", strings.Join(completed, ", "), strings.Join(evidence, ", "))
}
