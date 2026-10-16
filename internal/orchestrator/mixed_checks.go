package orchestrator

import (
	"path/filepath"
	"sort"
)

// A single-language mutation keeps targeted checks flexible. Mixed contracts
// acquire one command family per affected language so Python tests cannot
// certify a simultaneously changed Rust or TypeScript implementation.
func mixedLanguageChecks(runtime *taskRuntime, paths []string) []string {
	if runtime == nil {
		return nil
	}
	commands := map[string]bool{}
	for _, path := range paths {
		switch filepath.Ext(path) {
		case ".go":
			commands["go test"] = true
		case ".py":
			commands["pytest"] = true
		case ".rs":
			commands["cargo test"] = true
		case ".ts", ".tsx", ".js", ".jsx":
			commands["npm test"] = true
		}
	}
	if len(commands) < 2 {
		return nil
	}
	result := make([]string, 0, len(commands))
	for command := range commands {
		result = append(result, command)
	}
	sort.Strings(result)
	return result
}
