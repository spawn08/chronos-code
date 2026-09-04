package eval

import (
	"fmt"
	"time"
)

// TaskOutcomeVersion is the current task outcome JSON schema version.
const TaskOutcomeVersion = "v1"

// CallKind identifies the kind of execution call that produced usage.
type CallKind string

const (
	// CallModel is an LLM invocation, including retries and follow-ups.
	CallModel CallKind = "model"
	// CallTool is a tool invocation.
	CallTool CallKind = "tool"
)

// FailureClass distinguishes execution failures from grader failures.
type FailureClass string

const (
	// FailureExecution indicates the run could not complete.
	FailureExecution FailureClass = "execution"
	// FailureGrader indicates the run completed but did not satisfy the grader.
	FailureGrader FailureClass = "grader"
)

// Usage contains the resource use attributed to one correlated call.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CacheTokens  int     `json:"cache_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Call records one uniquely correlated model or tool invocation.
type Call struct {
	ID       string        `json:"id"`
	Kind     CallKind      `json:"kind"`
	Usage    Usage         `json:"usage"`
	Duration time.Duration `json:"duration_ns"`
}

// ModelSettings identifies the model configuration used for a task run.
type ModelSettings struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// VerificationEvidence records an executable verification run.
type VerificationEvidence struct {
	ID         string    `json:"id"`
	Command    string    `json:"command"`
	Passed     bool      `json:"passed"`
	ExecutedAt time.Time `json:"executed_at"`
}

// GraderOutcome records the external or deterministic task grade.
type GraderOutcome struct {
	Passed bool   `json:"passed"`
	Name   string `json:"name,omitempty"`
}

// Failure records why an unsuccessful task did not verify.
type Failure struct {
	Class   FailureClass `json:"class"`
	Message string       `json:"message"`
}

// TaskOutcome is the versioned, task-level IC-004 evaluation record.
type TaskOutcome struct {
	Version            string                 `json:"version"`
	TaskID             string                 `json:"task_id"`
	RunID              string                 `json:"run_id"`
	RepositoryRevision string                 `json:"repository_revision"`
	Model              ModelSettings          `json:"model"`
	Calls              []Call                 `json:"calls"`
	Verification       []VerificationEvidence `json:"verification"`
	LatestChangeAt     time.Time              `json:"latest_change_at"`
	Grader             GraderOutcome          `json:"grader"`
	Failure            *Failure               `json:"failure,omitempty"`
}

// Validate checks the invariants required before using an outcome in reports.
func (r TaskOutcome) Validate() error {
	if r.Version != TaskOutcomeVersion {
		return fmt.Errorf("unsupported task outcome version %q", r.Version)
	}
	if r.TaskID == "" || r.RunID == "" || r.RepositoryRevision == "" {
		return fmt.Errorf("task ID, run ID, and repository revision are required")
	}
	if r.Model.Provider == "" || r.Model.Name == "" {
		return fmt.Errorf("model provider and name are required")
	}

	callIDs := make(map[string]struct{}, len(r.Calls))
	for _, call := range r.Calls {
		if call.ID == "" {
			return fmt.Errorf("call ID is required")
		}
		if _, exists := callIDs[call.ID]; exists {
			return fmt.Errorf("duplicate call ID %q", call.ID)
		}
		callIDs[call.ID] = struct{}{}
		if call.Kind != CallModel && call.Kind != CallTool {
			return fmt.Errorf("call %s has unsupported kind %q", call.ID, call.Kind)
		}
	}

	if r.Grader.Passed {
		if r.Failure != nil {
			return fmt.Errorf("successful result cannot include a failure")
		}
		if len(r.Verification) == 0 {
			return fmt.Errorf("successful result requires executable verification evidence")
		}
		for _, evidence := range r.Verification {
			if evidence.ID == "" || evidence.Command == "" {
				return fmt.Errorf("verification evidence %s is not executable", evidence.ID)
			}
			if !evidence.Passed {
				return fmt.Errorf("successful result has failed verification evidence %s", evidence.ID)
			}
			if evidence.ExecutedAt.Before(r.LatestChangeAt) {
				return fmt.Errorf("verification evidence %s predates the latest change", evidence.ID)
			}
		}
	} else if r.Failure == nil {
		return fmt.Errorf("unsuccessful result requires a failure classification")
	}
	return nil
}

// Usage derives totals from the unique calls represented by this outcome.
func (r TaskOutcome) Usage() Usage {
	var total Usage
	seen := make(map[string]struct{}, len(r.Calls))
	for _, call := range r.Calls {
		if _, exists := seen[call.ID]; exists {
			continue
		}
		seen[call.ID] = struct{}{}
		total.InputTokens += call.Usage.InputTokens
		total.OutputTokens += call.Usage.OutputTokens
		total.CacheTokens += call.Usage.CacheTokens
		total.CostUSD += call.Usage.CostUSD
	}
	return total
}

// TokensPerVerifiedSuccess returns total input and output tokens divided by
// verified successes. Failed task usage remains in the numerator.
func TokensPerVerifiedSuccess(results []TaskOutcome) (float64, []string, error) {
	var tokens int
	var successes int
	failed := make([]string, 0)
	for _, result := range results {
		if err := result.Validate(); err != nil {
			return 0, nil, fmt.Errorf("task %s: %w", result.TaskID, err)
		}
		usage := result.Usage()
		tokens += usage.InputTokens + usage.OutputTokens
		if result.Grader.Passed {
			successes++
		} else {
			failed = append(failed, result.TaskID)
		}
	}
	if successes == 0 {
		return 0, failed, nil
	}
	return float64(tokens) / float64(successes), failed, nil
}
