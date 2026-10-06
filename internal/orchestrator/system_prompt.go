package orchestrator

import "strings"

// operatorPromptHeader marks caller-supplied instructions as the operator's
// and gives them priority over the built-in persona, not over security rules.
const operatorPromptHeader = "# Operator instructions\n\nThe operator who runs this session supplied the instructions below. When they conflict with the default guidance above (working style, autonomy, when to stop or ask, output format), follow these instructions. The security rules above still apply."

// AppendSystemPrompt appends caller-supplied instructions to the primary
// agent's system prompt under operatorPromptHeader. The existing prompt,
// including the security-floor text, is kept verbatim; the token-budget
// check applies only to shipped prompts, so the addition is never truncated.
func (o *Orchestrator) AppendSystemPrompt(text string) {
	text = strings.TrimSpace(text)
	if o == nil || text == "" {
		return
	}
	text = operatorPromptHeader + "\n\n" + text
	a, ok := o.agents[o.primary]
	if !ok || a == nil {
		return
	}
	if a.SystemPrompt == "" {
		a.SystemPrompt = text
		return
	}
	a.SystemPrompt = strings.TrimRight(a.SystemPrompt, "\n") + "\n\n" + text
}
