package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/verification"
)

func (o *Orchestrator) repairBlocking(ctx context.Context, a *agent.Agent, sessionID string, request ExecutionRequest, classification router.Classification, runtime *taskRuntime, response *model.ChatResponse) (*model.ChatResponse, verification.Decision, execution.StopReason, error) {
	decision := assessRuntimeVerification(request, classification, runtime)
	seen := make(map[string]struct{})
	for decision.Disagreement {
		fingerprint := verificationFailureFingerprint(decision)
		if _, repeated := seen[fingerprint]; repeated {
			if decision.Allowed {
				return response, decision, execution.StopSuccess, nil
			}
			return response, decision, execution.StopRepeatedFailure, nil
		}
		seen[fingerprint] = struct{}{}
		if err := runtime.budget.ConsumeRepairAttempt(); err != nil {
			return response, decision, execution.StopBudgetExhausted, err
		}
		repairPrompt := buildRepairPrompt(decision, runtime)
		var err error
		response, err = o.executeBlockingWithRecovery(ctx, a, sessionID, repairPrompt)
		if err != nil {
			return response, decision, execution.StopReasonForError(err), err
		}
		decision = assessRuntimeVerification(request, classification, runtime)
	}
	return response, decision, execution.StopSuccess, nil
}

func buildRepairPrompt(decision verification.Decision, runtime *taskRuntime) string {
	lines := []string{"Verification did not support completion. Repair only the unmet obligations below; do not replay the original task or repeat completed side effects."}
	paths := make(map[string]struct{})
	for _, obligation := range decision.Obligations {
		if obligation.Status == verification.StatusSatisfied {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s status=%s command=%q", obligation.ID, obligation.Status, obligation.Command))
		for _, path := range obligation.Paths {
			paths[path] = struct{}{}
		}
	}
	changed := make([]string, 0, len(paths))
	for path := range paths {
		changed = append(changed, path)
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		lines = append(lines, "Changed paths: "+strings.Join(changed, ", "))
	}
	lines = append(lines, runtime.claimsDigest()...)
	snapshot := runtime.budget.Snapshot()
	lines = append(lines, fmt.Sprintf("Remaining limits: repairs=%d model_calls=%d tool_calls=%d tokens=%d cost_microdollars=%d",
		remainingInt(snapshot.Limits.RepairAttempts, snapshot.RepairAttempts),
		remainingInt(snapshot.Limits.ModelCalls, snapshot.ModelCalls),
		remainingInt(snapshot.Limits.ToolCalls, snapshot.ToolCalls),
		remainingInt64(snapshot.Limits.Tokens, snapshot.Tokens),
		remainingInt64(snapshot.Limits.CostMicrodollars, snapshot.CostMicrodollars)))
	return strings.Join(lines, "\n")
}

func verificationFailureFingerprint(decision verification.Decision) string {
	parts := make([]string, 0, len(decision.Obligations))
	for _, obligation := range decision.Obligations {
		if obligation.Status != verification.StatusSatisfied {
			parts = append(parts, obligation.ID+":"+string(obligation.Status))
		}
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func remainingInt(limit, used int) int {
	if limit <= 0 {
		return 0
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func remainingInt64(limit, used int64) int64 {
	if limit <= 0 {
		return 0
	}
	if used >= limit {
		return 0
	}
	return limit - used
}
