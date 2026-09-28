package orchestrator

import (
	"context"
	"unicode/utf8"

	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

// EventToolOutput carries a tool's result before context compression (data:
// agent, id, tool, output) so UIs can show the real output while the model
// receives the compressed form. It is published before the tool_result event.
const EventToolOutput = "tool_output"

// maxToolOutputEventString bounds each string in an output event; the TUI
// caps inspection at 1 MiB and inline views far lower.
const maxToolOutputEventString = 256 << 10

// wrapToolOutputEvents must be installed inside the compression wrappers so
// it observes the handler's own result.
func wrapToolOutputEvents(a *agent.Agent) {
	if a == nil || a.Tools == nil {
		return
	}
	for _, definition := range a.Tools.List() {
		if definition == nil || definition.Handler == nil {
			continue
		}
		wrapped := *definition
		original := definition.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := original(ctx, args)
			if result != nil {
				publishToolOutput(ctx, a, wrapped.Name, result)
			}
			return result, err
		}
		a.Tools.Register(&wrapped)
	}
}

func publishToolOutput(ctx context.Context, a *agent.Agent, name string, result any) {
	broker := a.Broker
	callID, ok := agent.ToolCallIDFromContext(ctx)
	if broker == nil || !ok || callID == "" {
		return
	}
	event := chronosstream.Event{Type: EventToolOutput, Data: map[string]any{
		"agent": a.ID, "id": callID, "tool": name, "output": boundToolOutput(result),
	}}
	if session := storage.SessionFromContext(ctx); session != "" {
		broker.PublishTopic(session, event)
	} else {
		broker.Publish(event)
	}
}

// boundToolOutput caps top-level strings without mutating the result the
// model pipeline still owns.
func boundToolOutput(result any) any {
	switch value := result.(type) {
	case string:
		return capOutputString(value)
	case map[string]any:
		bounded := make(map[string]any, len(value))
		for key, field := range value {
			if text, ok := field.(string); ok {
				bounded[key] = capOutputString(text)
			} else {
				bounded[key] = field
			}
		}
		return bounded
	default:
		return result
	}
}

func capOutputString(s string) string {
	if len(s) <= maxToolOutputEventString {
		return s
	}
	s = s[:maxToolOutputEventString]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n… [output truncated for display]"
}
