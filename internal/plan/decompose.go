package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ID           NodeID      `json:"id"`
	DependsOn    []NodeID    `json:"depends_on"`
	Scope        string      `json:"scope"`
	ContextRefs  []ContextID `json:"context_refs"`
	Risks        []string    `json:"risks"`
	Verification string      `json:"verification"`
}

// PlannerOutput is the complete JSON object accepted from the PPD planner.
// Runtime plan identity is supplied separately by the caller.
type PlannerOutput struct {
	SourceRequestRef ContextID           `json:"source_request_ref"`
	ClassifierRef    ContextID           `json:"classifier_ref"`
	Nodes            []DecompositionNode `json:"nodes"`
}

// ParsePlannerOutput strictly decodes and validates one unwrapped JSON object.
func ParsePlannerOutput(data []byte) (PlannerOutput, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var output PlannerOutput
	if err := decoder.Decode(&output); err != nil {
		return PlannerOutput{}, fmt.Errorf("%w: decode planner output: %v", ErrInvalidDecomposition, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PlannerOutput{}, fmt.Errorf("%w: planner output must contain exactly one JSON object", ErrInvalidDecomposition)
	}
	if err := validateDecomposition(output.SourceRequestRef, output.ClassifierRef, output.Nodes); err != nil {
		return PlannerOutput{}, err
	}
	return output, nil
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
	if err := validateDecomposition(request.SourceRequestRef, request.ClassifierRef, request.Nodes); err != nil {
		return Plan{}, err
	}

	p := Plan{
		TenantID:     request.TenantID,
		RepositoryID: request.RepositoryID,
		TaskID:       request.TaskID,
		ID:           request.PlanID,
		Generation:   request.Generation,
		State:        PlanDraft,
	}
	for _, node := range request.Nodes {
		p.Nodes = append(p.Nodes, Node{ID: node.ID, State: NodePending, Scope: node.Scope, Risks: append([]string(nil), node.Risks...), Verification: node.Verification})
		for _, dependency := range node.DependsOn {
			edge := Dependency{NodeID: node.ID, DependsOn: dependency}
			p.Dependencies = append(p.Dependencies, edge)
		}
		for _, ref := range node.ContextRefs {
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

func validateDecomposition(sourceRequestRef, classifierRef ContextID, nodes []DecompositionNode) error {
	if strings.TrimSpace(string(sourceRequestRef)) == "" || strings.TrimSpace(string(classifierRef)) == "" {
		return fmt.Errorf("%w: missing source or classifier reference", ErrInvalidDecomposition)
	}
	if sourceRequestRef == classifierRef {
		return fmt.Errorf("%w: source and classifier references must differ", ErrInvalidDecomposition)
	}
	if len(nodes) == 0 || len(nodes) > MaxDecompositionNodes {
		return fmt.Errorf("%w: node count must be between 1 and %d", ErrInvalidDecomposition, MaxDecompositionNodes)
	}

	contextIDs := map[ContextID]struct{}{sourceRequestRef: {}, classifierRef: {}}
	dependencies := make(map[Dependency]struct{})
	graph := Plan{}
	for index, node := range nodes {
		if strings.TrimSpace(string(node.ID)) == "" {
			return fmt.Errorf("%w: node %d has no id", ErrInvalidDecomposition, index)
		}
		if node.DependsOn == nil {
			return fmt.Errorf("%w: node %q is missing depends_on", ErrInvalidDecomposition, node.ID)
		}
		if strings.TrimSpace(node.Scope) == "" {
			return fmt.Errorf("%w: node %q has no scope", ErrInvalidDecomposition, node.ID)
		}
		if node.ContextRefs == nil {
			return fmt.Errorf("%w: node %q is missing context_refs", ErrInvalidDecomposition, node.ID)
		}
		if len(node.Risks) == 0 {
			return fmt.Errorf("%w: node %q has no risks", ErrInvalidDecomposition, node.ID)
		}
		if strings.TrimSpace(node.Verification) == "" {
			return fmt.Errorf("%w: node %q has no verification", ErrInvalidDecomposition, node.ID)
		}
		for _, risk := range node.Risks {
			if strings.TrimSpace(risk) == "" {
				return fmt.Errorf("%w: node %q has an empty risk", ErrInvalidDecomposition, node.ID)
			}
		}

		graph.Nodes = append(graph.Nodes, Node{ID: node.ID})
		for _, dependency := range node.DependsOn {
			if strings.TrimSpace(string(dependency)) == "" {
				return fmt.Errorf("%w: node %q has an empty dependency", ErrInvalidDecomposition, node.ID)
			}
			edge := Dependency{NodeID: node.ID, DependsOn: dependency}
			if _, exists := dependencies[edge]; exists {
				return fmt.Errorf("%w: duplicate dependency %q -> %q", ErrInvalidDecomposition, edge.NodeID, edge.DependsOn)
			}
			dependencies[edge] = struct{}{}
			graph.Dependencies = append(graph.Dependencies, edge)
		}
		for _, ref := range node.ContextRefs {
			if strings.TrimSpace(string(ref)) == "" {
				return fmt.Errorf("%w: node %q has an empty context reference", ErrInvalidDecomposition, node.ID)
			}
			if _, exists := contextIDs[ref]; exists {
				return fmt.Errorf("%w: duplicate context reference %q", ErrInvalidDecomposition, ref)
			}
			contextIDs[ref] = struct{}{}
		}
	}
	if err := graph.ValidateDAG(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDecomposition, err)
	}
	return nil
}
