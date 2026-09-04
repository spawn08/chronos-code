// Package plan defines the durable runtime plan domain.
package plan

import "errors"

var (
	ErrInvalidPlanTransition = errors.New("invalid plan transition")
	ErrInvalidNodeTransition = errors.New("invalid node transition")
	ErrInvalidDAG            = errors.New("invalid plan DAG")
	ErrDuplicateID           = errors.New("duplicate stable ID")
	ErrImmutableGeneration   = errors.New("activated generation topology is immutable")
)

type (
	TenantID       string
	RepositoryID   string
	TaskID         string
	PlanID         string
	GenerationID   string
	NodeID         string
	AttemptID      string
	ContextID      string
	EvidenceID     string
	LeaseID        string
	EventID        string
	IdempotencyKey string
	PlanState      string
	NodeState      string
	StopReason     string
)

const (
	PlanDraft      PlanState = "draft"
	PlanActive     PlanState = "active"
	PlanPaused     PlanState = "paused"
	PlanReplanning PlanState = "replanning"
	PlanCompleted  PlanState = "completed"
	PlanFailed     PlanState = "failed"
	PlanCanceled   PlanState = "canceled"
)

const (
	NodeProposed  NodeState = "proposed"
	NodePending   NodeState = "pending"
	NodeReady     NodeState = "ready"
	NodeLeased    NodeState = "leased"
	NodeRunning   NodeState = "running"
	NodeRetryWait NodeState = "retry_wait"
	NodeBlocked   NodeState = "blocked"
	NodeCompleted NodeState = "completed"
	NodeFailed    NodeState = "failed"
	NodeCanceled  NodeState = "canceled"
)

const (
	StopAmbiguity            StopReason = "ambiguity"
	StopApprovalDenied       StopReason = "approval_denied"
	StopBudgetExhausted      StopReason = "budget_exhausted"
	StopVerificationFailed   StopReason = "verification_failed"
	StopCapabilityMissing    StopReason = "capability_missing"
	StopRetryExhausted       StopReason = "retry_exhausted"
	StopUserDecisionRequired StopReason = "user_decision_required"
)

// Plan is one immutable-topology generation of a durable task plan.
type Plan struct {
	TenantID     TenantID
	RepositoryID RepositoryID
	TaskID       TaskID
	ID           PlanID
	Generation   GenerationID
	State        PlanState
	StopReason   StopReason
	Nodes        []Node
	Dependencies []Dependency
	Attempts     []Attempt
	ContextRefs  []ContextRef
	Evidence     []Evidence
	Leases       []Lease
	Events       []Event
}

type Node struct {
	ID    NodeID
	State NodeState
}

// Dependency declares that NodeID cannot run until DependsOn is complete.
type Dependency struct {
	NodeID    NodeID
	DependsOn NodeID
}

type Attempt struct {
	ID             AttemptID
	NodeID         NodeID
	IdempotencyKey IdempotencyKey
}

type ContextRef struct {
	ID     ContextID
	NodeID NodeID
}

type Evidence struct {
	ID     EvidenceID
	NodeID NodeID
}

type Lease struct {
	ID        LeaseID
	AttemptID AttemptID
}

type Event struct {
	ID             EventID
	NodeID         NodeID
	IdempotencyKey IdempotencyKey
}

func (p *Plan) Transition(next PlanState) error {
	if !validPlanTransition(p.State, next) {
		return ErrInvalidPlanTransition
	}
	p.State = next
	return nil
}

func (n *Node) Transition(next NodeState) error {
	if !validNodeTransition(n.State, next) {
		return ErrInvalidNodeTransition
	}
	n.State = next
	return nil
}

func validPlanTransition(from, to PlanState) bool {
	switch from {
	case PlanDraft:
		return to == PlanActive
	case PlanActive:
		return to == PlanPaused || to == PlanReplanning || isTerminalPlanState(to)
	case PlanPaused:
		return to == PlanActive || isTerminalPlanState(to)
	case PlanReplanning:
		return to == PlanActive || isTerminalPlanState(to)
	}
	return false
}

func validNodeTransition(from, to NodeState) bool {
	switch from {
	case NodeProposed:
		return to == NodePending
	case NodePending:
		return to == NodeReady
	case NodeReady:
		return to == NodeLeased
	case NodeLeased:
		return to == NodeRunning || to == NodeRetryWait || to == NodeBlocked || to == NodeFailed || to == NodeCanceled
	case NodeRunning:
		return to == NodeRetryWait || to == NodeBlocked || to == NodeCompleted || to == NodeFailed || to == NodeCanceled
	case NodeRetryWait:
		return to == NodeReady
	case NodeBlocked:
		return to == NodeReady || to == NodeCanceled
	}
	return false
}

func isTerminalPlanState(state PlanState) bool {
	return state == PlanCompleted || state == PlanFailed || state == PlanCanceled
}

// ValidateDAG rejects duplicate node IDs, dangling dependencies, and cycles.
func (p Plan) ValidateDAG() error {
	nodes := make(map[NodeID]struct{}, len(p.Nodes))
	for _, node := range p.Nodes {
		if node.ID == "" {
			return ErrInvalidDAG
		}
		if _, exists := nodes[node.ID]; exists {
			return ErrDuplicateID
		}
		nodes[node.ID] = struct{}{}
	}

	dependencies := make(map[NodeID][]NodeID, len(p.Dependencies))
	for _, dependency := range p.Dependencies {
		if _, exists := nodes[dependency.NodeID]; !exists {
			return ErrInvalidDAG
		}
		if _, exists := nodes[dependency.DependsOn]; !exists || dependency.NodeID == dependency.DependsOn {
			return ErrInvalidDAG
		}
		dependencies[dependency.NodeID] = append(dependencies[dependency.NodeID], dependency.DependsOn)
	}

	visiting := make(map[NodeID]bool, len(nodes))
	visited := make(map[NodeID]bool, len(nodes))
	var visit func(NodeID) bool
	visit = func(id NodeID) bool {
		if visiting[id] {
			return false
		}
		if visited[id] {
			return true
		}
		visiting[id] = true
		for _, dependency := range dependencies[id] {
			if !visit(dependency) {
				return false
			}
		}
		visiting[id] = false
		visited[id] = true
		return true
	}
	for id := range nodes {
		if !visit(id) {
			return ErrInvalidDAG
		}
	}
	return nil
}

// DeriveTerminalState derives a terminal plan state without changing the plan.
func DeriveTerminalState(nodes []Node) (PlanState, bool) {
	if len(nodes) == 0 {
		return "", false
	}
	allCompleted := true
	canceled := false
	for _, node := range nodes {
		switch node.State {
		case NodeFailed:
			return PlanFailed, true
		case NodeCanceled:
			canceled = true
			allCompleted = false
		case NodeCompleted:
		default:
			allCompleted = false
		}
	}
	if allCompleted {
		return PlanCompleted, true
	}
	if canceled {
		return PlanCanceled, true
	}
	return "", false
}

// NewGeneration appends an active generation without modifying the replanning source.
func (p Plan) NewGeneration(generation GenerationID, nodes []Node, dependencies []Dependency) (Plan, error) {
	if p.State != PlanReplanning || generation == "" || generation == p.Generation {
		return Plan{}, ErrImmutableGeneration
	}

	nextNodes := make([]Node, 0, len(nodes)+len(p.Nodes))
	completed := make(map[NodeID]Node, len(p.Nodes))
	for _, node := range p.Nodes {
		if node.State == NodeCompleted {
			completed[node.ID] = node
		}
	}
	for _, node := range nodes {
		if preserved, ok := completed[node.ID]; ok {
			nextNodes = append(nextNodes, preserved)
			delete(completed, node.ID)
			continue
		}
		nextNodes = append(nextNodes, node)
	}
	for _, node := range completed {
		nextNodes = append(nextNodes, node)
	}

	next := Plan{
		TenantID:     p.TenantID,
		RepositoryID: p.RepositoryID,
		TaskID:       p.TaskID,
		ID:           p.ID,
		Generation:   generation,
		State:        PlanActive,
		Nodes:        nextNodes,
		Dependencies: append([]Dependency(nil), dependencies...),
		ContextRefs:  append([]ContextRef(nil), p.ContextRefs...),
	}
	preserved := make(map[NodeID]struct{}, len(nextNodes))
	for _, node := range nextNodes {
		if node.State == NodeCompleted {
			preserved[node.ID] = struct{}{}
		}
	}
	for _, evidence := range p.Evidence {
		if _, ok := preserved[evidence.NodeID]; ok {
			next.Evidence = append(next.Evidence, evidence)
		}
	}
	if err := next.ValidateDAG(); err != nil {
		return Plan{}, err
	}
	return next, nil
}
