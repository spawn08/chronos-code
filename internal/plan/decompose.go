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

const (
	MaxDecompositionNodes = 6
	maxNodeIDBytes        = 64
	maxContextIDBytes     = 128
	maxNodeTextBytes      = 512
	maxNodeListItems      = 8
)

var ErrInvalidDecomposition = errors.New("invalid plan decomposition")

// DecompositionRequest is the compact, metadata-only output accepted from the
// read-only delivery strategist. The reference IDs point to separately managed source
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
	ID                   NodeID        `json:"id"`
	Kind                 NodeKind      `json:"kind"`
	Objective            string        `json:"objective"`
	DependsOn            []NodeID      `json:"depends_on"`
	Scope                string        `json:"scope"`
	ContextRefs          []ContextID   `json:"context_refs"`
	ExpectedArtifacts    []string      `json:"expected_artifacts"`
	Assumptions          []string      `json:"assumptions"`
	InvalidationTriggers []string      `json:"invalidation_triggers"`
	RecoveryClass        RecoveryClass `json:"recovery_class"`
	Risks                []string      `json:"risks"`
	Verification         string        `json:"verification"`
}

// StrategistOutput is the complete JSON object accepted from the delivery strategist.
// Runtime plan identity is supplied separately by the caller.
type StrategistOutput struct {
	SourceRequestRef ContextID           `json:"source_request_ref"`
	ClassifierRef    ContextID           `json:"classifier_ref"`
	Nodes            []DecompositionNode `json:"nodes"`
}

// ParseStrategistOutput strictly decodes and validates one unwrapped JSON object.
func ParseStrategistOutput(data []byte) (StrategistOutput, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var output StrategistOutput
	if err := decoder.Decode(&output); err != nil {
		return StrategistOutput{}, fmt.Errorf("%w: decode strategist output: %v", ErrInvalidDecomposition, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return StrategistOutput{}, fmt.Errorf("%w: strategist output must contain exactly one JSON object", ErrInvalidDecomposition)
	}
	if err := validateDecomposition(output.SourceRequestRef, output.ClassifierRef, output.Nodes); err != nil {
		return StrategistOutput{}, err
	}
	return output, nil
}

// Decompose validates a strategist proposal, creates an inactive draft, and
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
		p.Nodes = append(p.Nodes, Node{
			ID: node.ID, State: NodePending, Kind: node.Kind, Objective: node.Objective, Scope: node.Scope,
			ExpectedArtifacts: append([]string(nil), node.ExpectedArtifacts...), Assumptions: append([]string(nil), node.Assumptions...),
			InvalidationTriggers: append([]string(nil), node.InvalidationTriggers...), RecoveryClass: node.RecoveryClass,
			Risks: append([]string(nil), node.Risks...), Verification: node.Verification,
		})
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
	if !boundedContextID(sourceRequestRef) || !boundedContextID(classifierRef) {
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
		if strings.TrimSpace(string(node.ID)) == "" || len(node.ID) > maxNodeIDBytes {
			return fmt.Errorf("%w: node %d has invalid id", ErrInvalidDecomposition, index)
		}
		if !validNodeKind(node.Kind) {
			return fmt.Errorf("%w: node %q has invalid kind %q", ErrInvalidDecomposition, node.ID, node.Kind)
		}
		if !boundedText(node.Objective) {
			return fmt.Errorf("%w: node %q has invalid objective", ErrInvalidDecomposition, node.ID)
		}
		if node.DependsOn == nil {
			return fmt.Errorf("%w: node %q is missing depends_on", ErrInvalidDecomposition, node.ID)
		}
		if len(node.DependsOn) > MaxDecompositionNodes {
			return fmt.Errorf("%w: node %q has too many dependencies", ErrInvalidDecomposition, node.ID)
		}
		if !boundedText(node.Scope) {
			return fmt.Errorf("%w: node %q has no scope", ErrInvalidDecomposition, node.ID)
		}
		if node.ContextRefs == nil {
			return fmt.Errorf("%w: node %q is missing context_refs", ErrInvalidDecomposition, node.ID)
		}
		if len(node.ContextRefs) > maxNodeListItems {
			return fmt.Errorf("%w: node %q has too many context_refs", ErrInvalidDecomposition, node.ID)
		}
		if err := validateStringList(node.ExpectedArtifacts, true); err != nil {
			return fmt.Errorf("%w: node %q has invalid expected_artifacts: %v", ErrInvalidDecomposition, node.ID, err)
		}
		if err := validateStringList(node.Assumptions, false); err != nil {
			return fmt.Errorf("%w: node %q has invalid assumptions: %v", ErrInvalidDecomposition, node.ID, err)
		}
		if err := validateStringList(node.InvalidationTriggers, false); err != nil {
			return fmt.Errorf("%w: node %q has invalid invalidation_triggers: %v", ErrInvalidDecomposition, node.ID, err)
		}
		if !validRecoveryClass(node.RecoveryClass) {
			return fmt.Errorf("%w: node %q has invalid recovery_class %q", ErrInvalidDecomposition, node.ID, node.RecoveryClass)
		}
		if err := validateStringList(node.Risks, true); err != nil {
			return fmt.Errorf("%w: node %q has no risks", ErrInvalidDecomposition, node.ID)
		}
		if !boundedText(node.Verification) {
			return fmt.Errorf("%w: node %q has no verification", ErrInvalidDecomposition, node.ID)
		}

		graph.Nodes = append(graph.Nodes, Node{ID: node.ID})
		for _, dependency := range node.DependsOn {
			if strings.TrimSpace(string(dependency)) == "" || len(dependency) > maxNodeIDBytes {
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
			if !boundedContextID(ref) {
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

func boundedText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maxNodeTextBytes
}

func boundedContextID(id ContextID) bool {
	return strings.TrimSpace(string(id)) != "" && len(id) <= maxContextIDBytes
}

func validateStringList(values []string, required bool) error {
	if values == nil || required && len(values) == 0 {
		return errors.New("missing list")
	}
	if len(values) > maxNodeListItems {
		return fmt.Errorf("more than %d items", maxNodeListItems)
	}
	for _, value := range values {
		if !boundedText(value) {
			return errors.New("empty or oversized item")
		}
	}
	return nil
}

func validNodeKind(kind NodeKind) bool {
	return kind == NodeInvestigate || kind == NodeDecide || kind == NodeImplement || kind == NodeVerify || kind == NodeIntegrate
}

func validRecoveryClass(class RecoveryClass) bool {
	return class == RecoveryRetry || class == RecoveryReplan || class == RecoveryDecide || class == RecoveryHalt
}
