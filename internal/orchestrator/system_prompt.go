package orchestrator

import "strings"

// AppendSystemPrompt appends caller-supplied instructions to the primary
// agent's system prompt. The existing prompt, including the security-floor
// text, is kept verbatim; the token-budget check applies only to shipped
// prompts, so the addition is never truncated.
func (o *Orchestrator) AppendSystemPrompt(text string) {
	text = strings.TrimSpace(text)
	if o == nil || text == "" {
		return
	}
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
