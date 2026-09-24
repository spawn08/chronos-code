package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrDeliveryNotFound           = errors.New("delivery not found")
	ErrInvalidDelivery            = errors.New("invalid delivery")
	ErrInvalidDeliveryTransition  = errors.New("invalid delivery transition")
	ErrStaleDeliveryVersion       = errors.New("stale delivery version")
	ErrAdmissionConflict          = errors.New("delivery admission key conflict")
	ErrEventConflict              = errors.New("delivery event idempotency conflict")
	ErrUnsupportedDeliverySchema  = errors.New("unsupported newer delivery schema")
	ErrIncompatibleDeliverySchema = errors.New("incompatible delivery schema")
)

type (
	TenantID               string
	RepositoryID           string
	DeliveryID             string
	AdmissionKey           string
	GoalRevision           int64
	RequirementID          string
	AcceptanceCheckID      string
	DecisionID             string
	DeliveryEventID        string
	DeliveryIdempotencyKey string
	DeliveryState          string
	RequirementStatus      string
	DeliveryEventType      string
)

const (
	DeliveryAdmitted           DeliveryState = "admitted"
	DeliveryQueued             DeliveryState = "queued"
	DeliveryRunning            DeliveryState = "running"
	DeliveryCheckpointing      DeliveryState = "checkpointing"
	DeliveryWaitingRetry       DeliveryState = "waiting_retry"
	DeliveryWaitingDecision    DeliveryState = "waiting_decision"
	DeliveryWaitingCredentials DeliveryState = "waiting_credentials"
	DeliveryWaitingQuota       DeliveryState = "waiting_quota"
	DeliveryPaused             DeliveryState = "paused"
	DeliveryReconciling        DeliveryState = "reconciling"
	DeliveryReplanning         DeliveryState = "replanning"
	DeliveryCancelRequested    DeliveryState = "cancel_requested"
	DeliverySucceeded          DeliveryState = "succeeded"
	DeliveryFailed             DeliveryState = "failed"
	DeliveryCancelled          DeliveryState = "cancelled"
)

const (
	RequirementAccepted  RequirementStatus = "accepted"
	RequirementSatisfied RequirementStatus = "satisfied"
	RequirementWaived    RequirementStatus = "waived"
)

const (
	DeliveryEventAdmitted           DeliveryEventType = "admitted"
	DeliveryEventTransitioned       DeliveryEventType = "transitioned"
	DeliveryEventGoalRevised        DeliveryEventType = "goal_revised"
	DeliveryEventRequirementUpdated DeliveryEventType = "requirement_updated"
	DeliveryEventDecisionRequested  DeliveryEventType = "decision_requested"
	DeliveryEventDecisionResolved   DeliveryEventType = "decision_resolved"
)

// DeliveryScope is the mandatory tenant and repository boundary for every query.
type DeliveryScope struct {
	TenantID     TenantID     `json:"tenant_id"`
	RepositoryID RepositoryID `json:"repository_id"`
}

type DeliveryRef struct {
	DeliveryID DeliveryID `json:"delivery_id"`
}

type Goal struct {
	Revision  GoalRevision `json:"revision"`
	Statement string       `json:"statement"`
	Actor     string       `json:"actor"`
	CreatedAt time.Time    `json:"created_at"`
}

type AcceptanceCheck struct {
	ID        AcceptanceCheckID `json:"id"`
	Statement string            `json:"statement"`
}

type RequirementWaiver struct {
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type Requirement struct {
	ID           RequirementID      `json:"id"`
	GoalRevision GoalRevision       `json:"goal_revision"`
	Statement    string             `json:"statement"`
	Status       RequirementStatus  `json:"status"`
	Checks       []AcceptanceCheck  `json:"checks"`
	Waiver       *RequirementWaiver `json:"waiver,omitempty"`
}

type DecisionResolution struct {
	Actor      string    `json:"actor"`
	Choice     string    `json:"choice"`
	Rationale  string    `json:"rationale"`
	ResolvedAt time.Time `json:"resolved_at"`
}

type Decision struct {
	ID                  DecisionID          `json:"id"`
	Question            string              `json:"question"`
	Options             []string            `json:"options"`
	Recommendation      string              `json:"recommendation"`
	Consequences        string              `json:"consequences"`
	Reversible          bool                `json:"reversible"`
	Deadline            *time.Time          `json:"deadline,omitempty"`
	BlockedDependencies []string            `json:"blocked_dependencies"`
	RequestedAt         time.Time           `json:"requested_at"`
	Resolution          *DecisionResolution `json:"resolution,omitempty"`
}

// Delivery is the durable, scoped aggregate. Version is advanced by every mutation.
type Delivery struct {
	DeliveryScope
	ID                  DeliveryID    `json:"id"`
	AdmissionKey        AdmissionKey  `json:"admission_key"`
	State               DeliveryState `json:"state"`
	Version             int64         `json:"version"`
	CurrentGoalRevision GoalRevision  `json:"current_goal_revision"`
	PolicyReference     string        `json:"policy_reference"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
	Goals               []Goal        `json:"goals"`
	Requirements        []Requirement `json:"requirements"`
	Decisions           []Decision    `json:"decisions"`
}

type DeliverySummary struct {
	ID                  DeliveryID    `json:"id"`
	State               DeliveryState `json:"state"`
	Version             int64         `json:"version"`
	CurrentGoalRevision GoalRevision  `json:"current_goal_revision"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
}

type DeliveryEvent struct {
	ID             DeliveryEventID        `json:"id"`
	Sequence       uint64                 `json:"sequence"`
	Type           DeliveryEventType      `json:"type"`
	IdempotencyKey DeliveryIdempotencyKey `json:"idempotency_key"`
	OccurredAt     time.Time              `json:"occurred_at"`
	Payload        json.RawMessage        `json:"payload"`
}

type EventIdentity struct {
	ID             DeliveryEventID
	IdempotencyKey DeliveryIdempotencyKey
	OccurredAt     time.Time
}

type Admission struct {
	Scope           DeliveryScope
	DeliveryID      DeliveryID
	AdmissionKey    AdmissionKey
	Goal            Goal
	Requirements    []Requirement
	PolicyReference string
	Event           EventIdentity
}

func (s DeliveryScope) Validate() error {
	if s.TenantID == "" || s.RepositoryID == "" {
		return ErrInvalidDelivery
	}
	return nil
}

func (d *Delivery) Transition(next DeliveryState) error {
	if !validDeliveryTransition(d.State, next) {
		return fmt.Errorf("%w: %s to %s", ErrInvalidDeliveryTransition, d.State, next)
	}
	d.State = next
	return nil
}

func validDeliveryTransition(from, to DeliveryState) bool {
	if from == to || isTerminalDeliveryState(from) {
		return false
	}
	switch from {
	case DeliveryAdmitted:
		return stateIn(to, DeliveryQueued, DeliveryPaused, DeliveryCancelRequested, DeliveryFailed)
	case DeliveryQueued:
		return stateIn(to, DeliveryRunning, DeliveryPaused, DeliveryReconciling, DeliveryReplanning, DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryCancelRequested, DeliveryFailed)
	case DeliveryRunning:
		return stateIn(to, DeliveryQueued, DeliveryCheckpointing, DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused, DeliveryReconciling, DeliveryReplanning, DeliveryCancelRequested, DeliverySucceeded, DeliveryFailed, DeliveryCancelled)
	case DeliveryCheckpointing:
		return stateIn(to, DeliveryQueued, DeliveryReconciling, DeliveryCancelRequested, DeliveryFailed)
	case DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused:
		return stateIn(to, DeliveryQueued, DeliveryCancelRequested, DeliveryFailed)
	case DeliveryReconciling:
		return stateIn(to, DeliveryQueued, DeliveryRunning, DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryReplanning, DeliveryCancelRequested, DeliverySucceeded, DeliveryFailed, DeliveryCancelled)
	case DeliveryReplanning:
		return stateIn(to, DeliveryQueued, DeliveryWaitingDecision, DeliveryCancelRequested, DeliveryFailed)
	case DeliveryCancelRequested:
		return stateIn(to, DeliveryReconciling, DeliveryCancelled, DeliveryFailed)
	default:
		return false
	}
}

func stateIn(state DeliveryState, allowed ...DeliveryState) bool {
	for _, candidate := range allowed {
		if state == candidate {
			return true
		}
	}
	return false
}

func isTerminalDeliveryState(state DeliveryState) bool {
	return stateIn(state, DeliverySucceeded, DeliveryFailed, DeliveryCancelled)
}

func validateRequirement(requirement Requirement, revision GoalRevision) error {
	if requirement.ID == "" || requirement.Statement == "" || requirement.Status == "" || requirement.GoalRevision != revision {
		return ErrInvalidDelivery
	}
	if !stateInRequirement(requirement.Status, RequirementAccepted, RequirementSatisfied, RequirementWaived) {
		return ErrInvalidDelivery
	}
	if (requirement.Status == RequirementWaived) != (requirement.Waiver != nil) {
		return ErrInvalidDelivery
	}
	if requirement.Waiver != nil && (requirement.Waiver.Actor == "" || requirement.Waiver.Reason == "") {
		return ErrInvalidDelivery
	}
	seen := make(map[AcceptanceCheckID]struct{}, len(requirement.Checks))
	for _, check := range requirement.Checks {
		if check.ID == "" || check.Statement == "" {
			return ErrInvalidDelivery
		}
		if _, exists := seen[check.ID]; exists {
			return ErrInvalidDelivery
		}
		seen[check.ID] = struct{}{}
	}
	return nil
}

func stateInRequirement(status RequirementStatus, allowed ...RequirementStatus) bool {
	for _, candidate := range allowed {
		if status == candidate {
			return true
		}
	}
	return false
}
