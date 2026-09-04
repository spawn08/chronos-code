package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const MaxDecompositionNodes = 12

var ErrInvalidDecomposition = errors.New("invalid plan decomposition")

// DecompositionRequest is the compact, metadata-only output accepted from the
// read-only PPD planner. The reference IDs point to separately managed source
// and classifier records rather than persisting raw prompts or evidence.
type DecompositionRequest struct {
	TenantID         TenantID
	RepositoryID     RepositoryID
	TaskID           TaskID
	PlanID           PlanID
	Generation       GenerationID
	SourceRequestRef ContextID
	ClassifierRef    ContextID
	Nodes            []DecompositionNode
}

// DecompositionNode describes one bounded, verifiable unit of planned work.
type DecompositionNode struct {
	ID           NodeID
	DependsOn    []NodeID
	Scope        string
	ContextRefs  []ContextID
	Risks        []string
	Verification string
}

// Decompose validates a planner proposal, creates an inactive draft, and
// persists the complete generation through the transactional plan store.
func Decompose(ctx context.Context, store *SQLStore, request DecompositionRequest) (Plan, error) {
	if store == nil {
		return Plan{}, fmt.Errorf("%w: missing plan store", ErrInvalidDecomposition)
	}
	if request.TenantID == "" || request.RepositoryID == "" || request.TaskID == "" || request.PlanID == "" || request.Generation == "" {
		return Plan{}, fmt.Errorf("%w: missing plan identity", ErrInvalidDecomposition)
	}
	if request.SourceRequestRef == "" || request.ClassifierRef == "" {
		return Plan{}, fmt.Errorf("%w: missing source or classifier reference", ErrInvalidDecomposition)
	}
	if request.SourceRequestRef == request.ClassifierRef {
		return Plan{}, fmt.Errorf("%w: source and classifier references must differ", ErrInvalidDecomposition)
	}
	if len(request.Nodes) == 0 || len(request.Nodes) > MaxDecompositionNodes {
		return Plan{}, fmt.Errorf("%w: node count must be between 1 and %d", ErrInvalidDecomposition, MaxDecompositionNodes)
	}

	p := Plan{
		TenantID:     request.TenantID,
		RepositoryID: request.RepositoryID,
		TaskID:       request.TaskID,
		ID:           request.PlanID,
		Generation:   request.Generation,
		State:        PlanDraft,
	}
	contextIDs := map[ContextID]struct{}{
		request.SourceRequestRef: {},
		request.ClassifierRef:    {},
	}
	dependencies := make(map[Dependency]struct{})
	for index, node := range request.Nodes {
		if node.ID == "" {
			return Plan{}, fmt.Errorf("%w: node %d has no id", ErrInvalidDecomposition, index)
		}
		if strings.TrimSpace(node.Scope) == "" {
			return Plan{}, fmt.Errorf("%w: node %q has no scope", ErrInvalidDecomposition, node.ID)
		}
		if strings.TrimSpace(node.Verification) == "" {
			return Plan{}, fmt.Errorf("%w: node %q has no verification", ErrInvalidDecomposition, node.ID)
		}
		if len(node.Risks) == 0 {
			return Plan{}, fmt.Errorf("%w: node %q has no risks", ErrInvalidDecomposition, node.ID)
		}
		for _, risk := range node.Risks {
			if strings.TrimSpace(risk) == "" {
				return Plan{}, fmt.Errorf("%w: node %q has an empty risk", ErrInvalidDecomposition, node.ID)
			}
		}

		p.Nodes = append(p.Nodes, Node{ID: node.ID, State: NodePending})
		for _, dependency := range node.DependsOn {
			edge := Dependency{NodeID: node.ID, DependsOn: dependency}
			if _, exists := dependencies[edge]; exists {
				return Plan{}, fmt.Errorf("%w: duplicate dependency %q -> %q", ErrInvalidDecomposition, edge.NodeID, edge.DependsOn)
			}
			dependencies[edge] = struct{}{}
			p.Dependencies = append(p.Dependencies, edge)
		}
		for _, ref := range node.ContextRefs {
			if ref == "" {
				return Plan{}, fmt.Errorf("%w: node %q has an empty context reference", ErrInvalidDecomposition, node.ID)
			}
			if _, exists := contextIDs[ref]; exists {
				return Plan{}, fmt.Errorf("%w: duplicate context reference %q", ErrInvalidDecomposition, ref)
			}
			contextIDs[ref] = struct{}{}
			p.ContextRefs = append(p.ContextRefs, ContextRef{ID: ref, NodeID: node.ID})
		}
	}
	firstNode := p.Nodes[0].ID
	p.ContextRefs = append(p.ContextRefs,
		ContextRef{ID: request.SourceRequestRef, NodeID: firstNode},
		ContextRef{ID: request.ClassifierRef, NodeID: firstNode},
	)
	if err := p.ValidateDAG(); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalidDecomposition, err)
	}
	if err := store.Create(ctx, p); err != nil {
		return Plan{}, fmt.Errorf("persist decomposed plan: %w", err)
	}
	return p, nil
}
