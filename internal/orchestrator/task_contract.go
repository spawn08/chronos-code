package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/defaults"
	"gopkg.in/yaml.v3"
)

// TaskMode controls completion guidance; it never grants tool permission.
type TaskMode string

const (
	TaskAnswer TaskMode = "answer"
	TaskAction TaskMode = "action"
)

// TaskContract lets a trusted caller supply request-local acceptance state.
// Missing contracts use conservative request hints and remain answer-only
// when the request does not clearly ask for action.
type TaskContract struct {
	Mode            TaskMode
	Outcome         string
	NonGoals        []string
	Acceptance      []string
	PendingDecision string
}

type taskContractKey struct{}

type contractPolicy struct {
	restricted, answer, action, pending []*regexp.Regexp
}

var loadContractPolicy = sync.OnceValues(func() (contractPolicy, error) {
	data, err := defaults.ReadFile("task-contract.yaml")
	if err != nil {
		return contractPolicy{}, fmt.Errorf("read task contract policy: %w", err)
	}
	return parseContractPolicy(data)
})

func parseContractPolicy(data []byte) (contractPolicy, error) {
	var input struct {
		Restricted []string `yaml:"restricted_patterns"`
		Answer     []string `yaml:"answer_patterns"`
		Action     []string `yaml:"action_patterns"`
		Pending    []string `yaml:"pending_decision_patterns"`
	}
	if err := yaml.Unmarshal(data, &input); err != nil {
		return contractPolicy{}, fmt.Errorf("parse task contract policy: %w", err)
	}
	var policy contractPolicy
	for _, group := range []struct {
		source []string
		target *[]*regexp.Regexp
	}{{input.Restricted, &policy.restricted}, {input.Answer, &policy.answer}, {input.Action, &policy.action}, {input.Pending, &policy.pending}} {
		for _, pattern := range group.source {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return policy, fmt.Errorf("compile task contract pattern: %w", err)
			}
			*group.target = append(*group.target, compiled)
		}
	}
	return policy, nil
}

func matchesContract(patterns []*regexp.Regexp, text string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func resolveTaskContract(request ExecutionRequest, roleID string, planOnly bool) (TaskContract, error) {
	policy, err := loadContractPolicy()
	if err != nil {
		return TaskContract{}, err
	}
	contract := TaskContract{Mode: TaskAnswer, Outcome: request.Message}
	if request.Contract != nil {
		contract = *request.Contract
		contract.NonGoals = append([]string(nil), contract.NonGoals...)
		contract.Acceptance = append([]string(nil), contract.Acceptance...)
		if contract.Outcome == "" {
			contract.Outcome = request.Message
		}
		if contract.Mode != TaskAnswer && contract.Mode != TaskAction {
			return TaskContract{}, fmt.Errorf("invalid task contract mode %q", contract.Mode)
		}
	} else if !matchesContract(policy.answer, request.Message) && matchesContract(policy.action, request.Message) {
		contract.Mode = TaskAction
	}
	// Role and explicit read-only constraints win over continuation hints.
	if planOnly || matchesContract(policy.restricted, request.Message) {
		contract.Mode = TaskAnswer
	}
	switch roleID {
	case "planner", "researcher", "explainer", "architect", "reviewer", "debugger", "security-auditor":
		contract.Mode = TaskAnswer
	}
	if len(contract.NonGoals) > 32 || len(contract.Acceptance) > 32 {
		return TaskContract{}, fmt.Errorf("task contract exceeds 32 constraints or acceptance checks")
	}
	for _, details := range [][]string{contract.NonGoals, contract.Acceptance, {contract.PendingDecision}} {
		for _, detail := range details {
			if len(detail) > 400 {
				return TaskContract{}, fmt.Errorf("task contract detail exceeds 400 bytes; use a bounded artifact reference")
			}
		}
	}
	return contract, nil
}

func withTaskContract(ctx context.Context, contract TaskContract) context.Context {
	return context.WithValue(ctx, taskContractKey{}, contract)
}

func taskContractFromContext(ctx context.Context) (TaskContract, bool) {
	contract, ok := ctx.Value(taskContractKey{}).(TaskContract)
	return contract, ok
}

func continuationAllowed(ctx context.Context, finalText string) bool {
	contract, ok := taskContractFromContext(ctx)
	if !ok || contract.Mode != TaskAction || contract.PendingDecision != "" {
		return false
	}
	policy, err := loadContractPolicy()
	return err == nil && !matchesContract(policy.pending, finalText)
}

func taskContractDigest(ctx context.Context) string {
	contract, ok := taskContractFromContext(ctx)
	if !ok {
		return ""
	}
	var text strings.Builder
	outcome := validPrefix(contract.Outcome, 2000)
	if len(outcome) < len(contract.Outcome) {
		outcome += "\n[Outcome excerpt; consult the original user request before continuing.]"
	}
	fmt.Fprintf(&text, "[Live task contract; completion guidance only, no new action authorization]\nmode=%s\nAuthorized outcome: %s\n", contract.Mode, outcome)
	for _, value := range contract.NonGoals {
		fmt.Fprintf(&text, "Non-goal/constraint: %s\n", validPrefix(value, 400))
	}
	for _, value := range contract.Acceptance {
		fmt.Fprintf(&text, "Acceptance evidence required: %s\n", validPrefix(value, 400))
	}
	if contract.PendingDecision != "" {
		fmt.Fprintf(&text, "Pending decision: %s\n", validPrefix(contract.PendingDecision, 400))
	}
	return text.String()
}
