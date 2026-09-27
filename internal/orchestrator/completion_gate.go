package orchestrator

import (
	"context"
	"regexp"
	"strings"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

// maxCompletionNudges bounds how often one user turn is sent back to work
// after the model tries to end it with the task visibly unfinished.
const maxCompletionNudges = 3

// completionGate tracks nudges within one user turn. A turn ends normally when
// the model's reply to a nudge made no tool calls: it is then either done or
// genuinely waiting on the user, and nudging again would only loop.
type completionGate struct {
	nudges           int
	toolCallsAtNudge int
}

// handBackPatterns match a final paragraph that announces or defers work
// instead of doing it. Offers of optional follow-ups after finished work
// ("let me know if you'd like...") deliberately do not match.
var handBackPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(reply|say|type|respond)( with)? ["'` + "`" + `]?(continue|go ahead|proceed|yes)\b`),
	regexp.MustCompile(`\b(next|new|another|follow-up|separate) (session|conversation|turn)\b`),
	regexp.MustCompile(`\bshall i\b`),
	regexp.MustCompile(`\bshould i (proceed|go ahead|continue|start|begin|implement|apply|write|create|run)\b`),
	regexp.MustCompile(`\b(do you want|would you like|if you('d like| would like| want)) me to (proceed|continue|go ahead|start|begin|implement|apply)\b`),
	regexp.MustCompile(`\blet me know (when|if) (i should|to) (proceed|continue|start|go ahead)\b`),
	regexp.MustCompile(`\b(next|now),? i('ll| will)\b`),
	regexp.MustCompile(`\bi('ll| will) (now|next)\b`),
	regexp.MustCompile(`(^|[.!:\n] *)next (is|up|step is)\b`),
	regexp.MustCompile(`(^|[.!:\n] *)let me (now |next )?(write|create|add|build|implement|run|fix|update|start|finish|continue)\b`),
}

// handBackPhrase returns the hand-back phrase in the final paragraph, if any.
func handBackPhrase(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len(text) > 600 {
		text = text[len(text)-600:]
	}
	text = strings.NewReplacer("\u2019", "'", "\u2018", "'", "\u201c", `"`, "\u201d", `"`).Replace(strings.ToLower(text))
	for _, pattern := range handBackPatterns {
		if match := pattern.FindString(text); match != "" {
			return strings.TrimSpace(strings.TrimLeft(match, ".!:\n "))
		}
	}
	return ""
}

// completionNudge returns a prompt that sends the model back to work when its
// turn ended with open plan items or a hand-back, or "" to let the turn end.
func (o *Orchestrator) completionNudge(ctx context.Context, a *agent.Agent, sessionID string, gate *completionGate, stop model.StopReason, finalText string, toolCalls int) string {
	if gate == nil || gate.nudges >= maxCompletionNudges || o.PlanMode() || !canContinueOutput(a, sessionID) {
		return ""
	}
	switch stop {
	case model.StopReasonEnd, model.StopReasonMaxTokens, "":
	default:
		return ""
	}
	if gate.nudges > 0 && toolCalls == gate.toolCallsAtNudge {
		return ""
	}
	open := o.taskPlans.openItems(ctx, sessionID)
	phrase := handBackPhrase(finalText)
	if len(open) == 0 && phrase == "" {
		return ""
	}
	gate.nudges++
	gate.toolCallsAtNudge = toolCalls
	return buildCompletionNudge(open, phrase)
}

func buildCompletionNudge(open []builtins.PlanTask, phrase string) string {
	lines := []string{"Your turn ended before the task was complete."}
	if len(open) > 0 {
		lines = append(lines, "Your plan still has unfinished items:")
		for _, task := range open {
			lines = append(lines, "- ["+string(task.Status)+"] "+task.Content)
		}
	}
	if phrase != "" {
		lines = append(lines, `Your last message announces or hands back work instead of doing it ("`+phrase+`").`)
	}
	lines = append(lines,
		"The user is not watching and cannot answer mid-task. Do the remaining work now with tool calls: finish the open items, "+
			"or remove items that are no longer needed with update_plan. Do not ask permission for reversible steps within the original request, "+
			"and do not ask the user to reply \"continue\" or to start a new session; context is compacted automatically.",
		"End your turn only when the task is complete, or when you are blocked on input only the user can provide; then state exactly what you need.")
	return strings.Join(lines, "\n")
}

func (r *taskRuntime) toolCallCount() int {
	if r == nil || r.budget == nil {
		return 0
	}
	return r.budget.Snapshot().ToolCalls
}
