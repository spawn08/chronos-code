package plan

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/spawn08/chronos-code/internal/worktree"
)

// NodeExecutor runs one leased plan node. Implementations must make external
// effects idempotent using the attempt identity supplied in the request.
type NodeExecutor interface {
	Execute(context.Context, NodeExecutionRequest) (NodeExecutionResult, error)
}

// NodeExecutionRequest contains only the persisted plan metadata and bounded
// restart context needed to resume a node.
type NodeExecutionRequest struct {
	Plan    Plan
	Node    Node
	Attempt AttemptID
	Context RestartContext
}

type VerificationState string

const (
	VerificationPending VerificationState = "pending"
	VerificationPassed  VerificationState = "passed"
	VerificationFailed  VerificationState = "failed"
)

// NodeExecutionResult is the terminal, durable-planning outcome of one attempt.
// A node is complete only when Status and Verification explicitly report success.
type NodeExecutionResult struct {
	NodeID              NodeID
	AttemptID           AttemptID
	Status              NodeState
	StopReason          StopReason
	Summary             string
	ChangedPaths        []string
	EvidenceIDs         []EvidenceID
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CostMicrodollars    int64
	Verification        VerificationState
	Workspace           *worktree.Result
}

// ContextLoader resolves persisted context references without retaining prior
// conversation content in the controller.
type ContextLoader func(context.Context, Plan, Node) ([]ContextEntry, error)

// NodeVerifier validates a completed execution before its node is committed.
type NodeVerifier interface {
	Verify(context.Context, Plan, Node, NodeExecutionResult) (NodeExecutionResult, error)
}

type legacyNodeExecutor interface {
	Execute(context.Context, NodeExecutionRequest) error
}

type legacyNodeVerifier interface {
	Verify(context.Context, Plan, Node) error
}

// NodeAccess describes the paths an executor may modify. Read-only nodes may
// run together; writes with a shared path are serialized by the controller.
type NodeAccess struct {
	ReadOnly bool
	Paths    []string
}

// NodeAccessProvider is optional. Executors that do not provide access
// metadata are run one at a time, which is the conservative safe behavior.
type NodeAccessProvider interface {
	Access(context.Context, Plan, Node) (NodeAccess, error)
}

// StopError lets an executor report one of the durable autonomous stop
// conditions instead of treating it as a retryable execution error.
type StopError struct {
	Reason StopReason
	Err    error
}

func (e *StopError) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return e.Err.Error()
}

func (e *StopError) Unwrap() error { return e.Err }

// ControllerConfig configures execution bounds that are independent of the
// persisted plan topology.
type ControllerConfig struct {
	Scheduler    SchedulerConfig
	ContextBytes int
}

// Controller advances a durable plan generation until it completes or reaches
// a persisted stop state.
type Controller struct {
	store          *SQLStore
	scheduler      *Scheduler
	executor       NodeExecutor
	accessProvider NodeAccessProvider
	loadContext    ContextLoader
	verifier       NodeVerifier
	contextBytes   int
}

func NewController(store *SQLStore, executor any, loadContext ContextLoader, verifier any, config ControllerConfig) *Controller {
	return &Controller{
		store:          store,
		scheduler:      NewScheduler(store, config.Scheduler),
		executor:       adaptNodeExecutor(executor),
		accessProvider: adaptNodeAccessProvider(executor),
		loadContext:    loadContext,
		verifier:       adaptNodeVerifier(verifier),
		contextBytes:   config.ContextBytes,
	}
}

func adaptNodeAccessProvider(executor any) NodeAccessProvider {
	provider, _ := executor.(NodeAccessProvider)
	return provider
}

func adaptNodeExecutor(executor any) NodeExecutor {
	if current, ok := executor.(NodeExecutor); ok {
		return current
	}
	if legacy, ok := executor.(legacyNodeExecutor); ok {
		return nodeExecutorFunc(func(ctx context.Context, request NodeExecutionRequest) (NodeExecutionResult, error) {
			err := legacy.Execute(ctx, request)
			return NodeExecutionResult{NodeID: request.Node.ID, AttemptID: request.Attempt, Status: NodeCompleted, Verification: VerificationPassed}, err
		})
	}
	return nil
}

func adaptNodeVerifier(verifier any) NodeVerifier {
	if current, ok := verifier.(NodeVerifier); ok {
		return current
	}
	if legacy, ok := verifier.(legacyNodeVerifier); ok {
		return nodeVerifierFunc(func(ctx context.Context, p Plan, node Node, result NodeExecutionResult) (NodeExecutionResult, error) {
			err := legacy.Verify(ctx, p, node)
			if err != nil {
				result.Verification = VerificationFailed
			} else {
				result.Verification = VerificationPassed
			}
			return result, err
		})
	}
	return nil
}

type nodeExecutorFunc func(context.Context, NodeExecutionRequest) (NodeExecutionResult, error)

func (f nodeExecutorFunc) Execute(ctx context.Context, request NodeExecutionRequest) (NodeExecutionResult, error) {
	return f(ctx, request)
}

type nodeVerifierFunc func(context.Context, Plan, Node, NodeExecutionResult) (NodeExecutionResult, error)

func (f nodeVerifierFunc) Verify(ctx context.Context, p Plan, node Node, result NodeExecutionResult) (NodeExecutionResult, error) {
	return f(ctx, p, node, result)
}

// Decompose persists a draft using the controller's durable store.
func (c *Controller) Decompose(ctx context.Context, request DecompositionRequest) (Plan, error) {
	if c.store == nil {
		return Plan{}, fmt.Errorf("decompose plan: missing plan store")
	}
	return Decompose(ctx, c.store, request)
}

// Run resumes an active generation. A draft is activated before its first
// claim; paused and terminal plans are returned unchanged.
func (c *Controller) Run(ctx context.Context, p Plan) (Plan, error) {
	if c.store == nil || c.scheduler == nil || c.executor == nil {
		return Plan{}, fmt.Errorf("run plan: missing controller dependency")
	}
	for {
		current, err := c.store.Load(ctx, p)
		if err != nil {
			return Plan{}, err
		}
		if current.State == PlanDraft {
			version, err := c.store.Version(ctx, current)
			if err != nil {
				return Plan{}, err
			}
			if err := c.store.TransitionPlan(ctx, current, version, PlanActive); err != nil {
				return Plan{}, err
			}
			continue
		}
		if current.State != PlanActive {
			return current, nil
		}

		ready, err := c.scheduler.Ready(ctx, current)
		if err != nil {
			return Plan{}, err
		}
		if len(ready) == 0 {
			return current, nil
		}
		for _, batch := range c.batches(ctx, current, ready) {
			if err := c.runBatch(ctx, current, batch); err != nil {
				return Plan{}, err
			}
			updated, err := c.store.Load(ctx, current)
			if err != nil {
				return Plan{}, err
			}
			if updated.State != PlanActive {
				return updated, nil
			}
		}
	}
}

func (c *Controller) batches(ctx context.Context, p Plan, nodes []Node) [][]Node {
	provider := c.accessProvider
	if provider == nil {
		batches := make([][]Node, len(nodes))
		for i, node := range nodes {
			batches[i] = []Node{node}
		}
		return batches
	}
	var batches [][]Node
	var writes []map[string]struct{}
	for _, node := range nodes {
		access, err := provider.Access(ctx, p, node)
		if err != nil {
			batches = append(batches, []Node{node})
			writes = append(writes, map[string]struct{}{"": {}})
			continue
		}
		paths := make(map[string]struct{}, len(access.Paths))
		for _, path := range access.Paths {
			paths[path] = struct{}{}
		}
		if !access.ReadOnly && len(paths) == 0 {
			paths[""] = struct{}{}
		}
		for i, owned := range writes {
			if access.ReadOnly || !overlaps(paths, owned) {
				batches[i] = append(batches[i], node)
				if !access.ReadOnly {
					for path := range paths {
						owned[path] = struct{}{}
					}
				}
				goto assigned
			}
		}
		batches = append(batches, []Node{node})
		writes = append(writes, paths)
	assigned:
	}
	return batches
}

func overlaps(first, second map[string]struct{}) bool {
	if _, wildcard := first[""]; wildcard {
		return len(second) > 0
	}
	if _, wildcard := second[""]; wildcard {
		return len(first) > 0
	}
	for path := range first {
		if _, ok := second[path]; ok {
			return true
		}
	}
	return false
}

func (c *Controller) runBatch(ctx context.Context, p Plan, nodes []Node) error {
	type claim struct {
		node    Node
		request ClaimRequest
	}
	claims := make([]claim, 0, len(nodes))
	for _, expected := range nodes {
		claimed, request, err := c.claim(ctx, p, expected)
		if errors.Is(err, ErrNoReadyNode) {
			continue
		}
		if err != nil {
			return err
		}
		claims = append(claims, claim{node: claimed, request: request})
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(claims))
	for _, claimed := range claims {
		wg.Add(1)
		go func(claimed claim) {
			defer wg.Done()
			errs <- c.runClaimed(ctx, p, claimed.node, claimed.request)
		}(claimed)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) claim(ctx context.Context, p Plan, expected Node) (Node, ClaimRequest, error) {
	current, err := c.store.Load(ctx, p)
	if err != nil {
		return Node{}, ClaimRequest{}, err
	}
	attempt := 1
	for _, previous := range current.Attempts {
		if previous.NodeID == expected.ID {
			attempt++
		}
	}
	suffix := fmt.Sprintf("%s-%d", expected.ID, attempt)
	request := ClaimRequest{AttemptID: AttemptID("controller-" + suffix), LeaseID: LeaseID("controller-" + suffix), EventID: EventID("controller-" + suffix), IdempotencyKey: IdempotencyKey("controller-" + suffix)}
	claimed, err := c.scheduler.Claim(ctx, p, request)
	if err != nil {
		return Node{}, ClaimRequest{}, err
	}
	if claimed.ID != expected.ID {
		return Node{}, ClaimRequest{}, fmt.Errorf("claim plan node: got %q, want %q", claimed.ID, expected.ID)
	}
	return claimed, request, nil
}

func (c *Controller) runClaimed(ctx context.Context, p Plan, claimed Node, request ClaimRequest) error {
	if err := c.scheduler.Start(ctx, p, claimed.ID, request.LeaseID); err != nil {
		return err
	}
	var entries []ContextEntry
	var err error
	if c.loadContext != nil {
		entries, err = c.loadContext(ctx, p, claimed)
		if err != nil {
			return c.stopClaimed(ctx, p, claimed.ID, request, StopAmbiguity)
		}
	}
	restart, err := BuildRestartContext(entries, c.contextBytes, "")
	var result NodeExecutionResult
	if err == nil {
		result, err = c.executor.Execute(ctx, NodeExecutionRequest{Plan: p, Node: claimed, Attempt: request.AttemptID, Context: restart})
	}
	var stopped *StopError
	if errors.As(err, &stopped) {
		return c.stopClaimed(ctx, p, claimed.ID, request, stopped.Reason)
	}
	if err != nil {
		return c.retry(ctx, p, claimed.ID, request)
	}
	if err := validateNodeExecutionResult(result, claimed.ID, request.AttemptID); err != nil {
		return c.stopClaimed(ctx, p, claimed.ID, request, StopAmbiguity)
	}
	if result.Status != NodeCompleted {
		return c.persistTerminalResult(ctx, p, claimed.ID, request, result)
	}
	if c.verifier != nil {
		result, err = c.verifier.Verify(ctx, p, claimed, result)
		if err != nil {
			return c.stopClaimed(ctx, p, claimed.ID, request, StopVerificationFailed)
		}
		if err := validateNodeExecutionResult(result, claimed.ID, request.AttemptID); err != nil {
			return c.stopClaimed(ctx, p, claimed.ID, request, StopVerificationFailed)
		}
		if result.Status != NodeCompleted {
			return c.persistTerminalResult(ctx, p, claimed.ID, request, result)
		}
	}
	if result.Status != NodeCompleted || result.StopReason != "" || result.Verification != VerificationPassed {
		return c.stopClaimed(ctx, p, claimed.ID, request, StopVerificationFailed)
	}
	return c.scheduler.CompleteWithEvidence(ctx, p, claimed.ID, request.LeaseID, EventID("complete-"+string(request.AttemptID)), IdempotencyKey("complete-"+string(request.AttemptID)), result.EvidenceIDs)
}

func validateNodeExecutionResult(result NodeExecutionResult, nodeID NodeID, attemptID AttemptID) error {
	if result.NodeID != nodeID || result.AttemptID != attemptID {
		return fmt.Errorf("node execution result identity mismatch")
	}
	switch result.Status {
	case NodeCompleted, NodeFailed, NodeBlocked, NodeCanceled:
		return nil
	default:
		return fmt.Errorf("node execution result is not terminal")
	}
}

func (c *Controller) retry(ctx context.Context, p Plan, nodeID NodeID, request ClaimRequest) error {
	if err := c.scheduler.Retry(ctx, p, nodeID, request.LeaseID, EventID("retry-"+string(request.AttemptID)), IdempotencyKey("retry-"+string(request.AttemptID))); err != nil {
		return err
	}
	loaded, err := c.store.Load(ctx, p)
	if err != nil {
		return err
	}
	if loaded.State == PlanFailed {
		return c.stop(ctx, p, StopRetryExhausted)
	}
	return nil
}

func (c *Controller) persistTerminalResult(ctx context.Context, p Plan, nodeID NodeID, request ClaimRequest, result NodeExecutionResult) error {
	if result.StopReason == "" {
		return c.stopClaimed(ctx, p, nodeID, request, StopAmbiguity)
	}
	return c.scheduler.Stop(ctx, p, nodeID, request.LeaseID, EventID("stop-"+string(request.AttemptID)), IdempotencyKey("stop-"+string(request.AttemptID)), result.Status, result.StopReason)
}

func (c *Controller) stopClaimed(ctx context.Context, p Plan, nodeID NodeID, request ClaimRequest, reason StopReason) error {
	status := NodeBlocked
	if reason == StopVerificationFailed || reason == StopRetryExhausted {
		status = NodeFailed
	}
	return c.scheduler.Stop(ctx, p, nodeID, request.LeaseID, EventID("stop-"+string(request.AttemptID)), IdempotencyKey("stop-"+string(request.AttemptID)), status, reason)
}

func (c *Controller) stop(ctx context.Context, p Plan, reason StopReason) error {
	state := PlanPaused
	if reason == StopVerificationFailed || reason == StopRetryExhausted {
		state = PlanFailed
	}
	_, err := c.store.db.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?, stop_reason = ?, version = version + 1`), append([]any{state, reason}, planArgs(p)...)...)
	if err != nil {
		return fmt.Errorf("stop plan: %w", err)
	}
	return nil
}
