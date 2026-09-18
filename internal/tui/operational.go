package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

func RenderOperationalSnapshot(snapshot orchestrator.OperationalSnapshot, width int) string {
	lines := []string{
		fmt.Sprintf("safety: permission=%s · plan-only=%t · verification=%s", emptyLabel(snapshot.PermissionMode), snapshot.PlanOnly, emptyLabel(string(snapshot.VerificationMode))),
		"task: " + renderExecutionState(snapshot.Execution),
		"limits remaining: " + renderRemainingLimits(snapshot.Execution),
		"plan: " + renderPlanState(snapshot.Plan),
		"active specialists: " + renderSpecialists(snapshot.ActiveSpecialists),
		"worktree IDs: " + emptyList(snapshot.WorktreeIDs),
		"VCS changed paths: " + emptyList(snapshot.ChangedPaths),
		"checkpoint history: " + renderCheckpoints(snapshot.Checkpoints),
	}
	for i := range lines {
		lines[i] = wrapText(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

func renderExecutionState(snapshot orchestrator.ExecutionSnapshot) string {
	if snapshot.TaskID == "" {
		return "none · verification=idle"
	}
	state := "stopped:" + string(snapshot.StopReason)
	if snapshot.Running {
		state = "running"
	}
	return fmt.Sprintf("%s · %s · verification=%s", snapshot.TaskID, state, operationalVerification(snapshot))
}

func renderRemainingLimits(snapshot orchestrator.ExecutionSnapshot) string {
	budget := snapshot.Budget
	var values []string
	add := func(name string, limit, used int64) {
		if limit > 0 {
			values = append(values, fmt.Sprintf("%s=%d", name, max(int64(0), limit-used)))
		}
	}
	add("repair", int64(budget.Limits.RepairAttempts), int64(budget.RepairAttempts))
	add("model", int64(budget.Limits.ModelCalls), int64(budget.ModelCalls))
	add("tool", int64(budget.Limits.ToolCalls), int64(budget.ToolCalls))
	add("tokens", budget.Limits.Tokens, budget.Tokens)
	add("cost", budget.Limits.CostMicrodollars, budget.CostMicrodollars)
	if budget.Limits.WallTime > 0 {
		remaining := budget.Limits.WallTime - budget.Elapsed
		if remaining < 0 {
			remaining = 0
		}
		values = append(values, "time="+remaining.Round(time.Second).String())
	}
	if len(values) == 0 {
		return "unlimited"
	}
	if budget.ExhaustedDimension != "" {
		values = append(values, "exhausted="+string(budget.ExhaustedDimension))
	}
	return strings.Join(values, " · ")
}

func renderPlanState(snapshot orchestrator.PlanSnapshot) string {
	if snapshot.ID == "" {
		return "none"
	}
	states := make(map[string]int)
	for _, node := range snapshot.Nodes {
		states[string(node.State)]++
	}
	keys := make([]string, 0, len(states))
	for state := range states {
		keys = append(keys, state)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, state := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", state, states[state]))
	}
	result := fmt.Sprintf("%s · %s", snapshot.ID, snapshot.State)
	if snapshot.StopReason != "" {
		result += " · stop=" + string(snapshot.StopReason)
	}
	if len(parts) > 0 {
		result += " · " + strings.Join(parts, ",")
	}
	return result
}

func renderSpecialists(values []orchestrator.SpecialistSnapshot) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, "@"+value.AgentID+"("+value.TaskID+")")
	}
	return emptyList(items)
}

func renderCheckpoints(values []orchestrator.CheckpointSnapshot) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, value.Path+"("+value.State+")")
	}
	return emptyList(items)
}

func emptyList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func emptyLabel(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
