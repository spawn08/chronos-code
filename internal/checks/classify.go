package checks

import (
	"github.com/spawn08/chronos-code/internal/execution"
	"gopkg.in/yaml.v3"
)

// ClassifyPackageCommand identifies registered script names; Infer validates
// the actual script and its coverage before any receipt is accepted.
func ClassifyPackageCommand(command string) (execution.CommandClass, bool) {
	var reg registry
	if err := yaml.Unmarshal(registryYAML, &reg); err != nil {
		return execution.CommandUnknown, false
	}
	tokens, ok := words(command, reg.Limits.CommandBytes)
	if !ok {
		return execution.CommandUnknown, false
	}
	tokens, ok = normalize(tokens)
	if !ok || len(tokens) < 2 || !contains(reg.Runners["javascript"], tokens[0]) {
		return execution.CommandUnknown, false
	}
	args := tokens[1:]
	if tokens[0] == "bun" && args[0] == "test" {
		return execution.CommandTest, true
	}
	if args[0] == "run" || tokens[0] == "npm" && args[0] == "run-script" {
		args = args[1:]
	} else if tokens[0] == "bun" || tokens[0] == "npm" && args[0] != "test" {
		return execution.CommandUnknown, false
	}
	if len(args) == 0 {
		return execution.CommandUnknown, false
	}
	for _, script := range reg.Scripts {
		if script.Name == args[0] {
			return script.Class, true
		}
	}
	return execution.CommandUnknown, false
}
