package orchestrator

import (
	"strings"
	"testing"
)

func TestStripHarnessContext(t *testing.T) {
	nested := "<turn_context>\nRelevant context from prior sessions:\n- user: <turn_context>\nold\n</turn_context>\n\nolder ask\n</turn_context>\n\n" +
		"Path: complexity=low kind=edit suggested_graph=L2 suggested_plan=skip. Advisory only.\n\nfix the OI chain"
	tests := []struct {
		name, in, want string
	}{
		{"plain text unchanged", "deploy to AKS", "deploy to AKS"},
		{"nested turn context and routing hint removed", nested, "fix the OI chain"},
		{"unterminated block dropped", "keep <turn_context> partial", "keep"},
		{"harness note dropped", "run the tests\n\n[harness note] Futility check: x", "run the tests"},
		{"ppd and routing lines removed", "Routing hint: intent=debug specialist=debugger.\nPPD hint: plan\nreal ask", "real ask"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.TrimSpace(stripHarnessContext(tt.in)); got != tt.want {
				t.Errorf("stripHarnessContext() = %q, want %q", got, tt.want)
			}
		})
	}
}
