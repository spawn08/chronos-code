package eval

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/defaults"
)

// systemPromptTokenBudget mirrors internal/cli.systemPromptTokenBudget (PRD
// P1-005/G-012's "<800 tokens" target for the base system prompt).
const systemPromptTokenBudget = 800

// Summary aggregates a suite run for the CI gate and the dashboard.
type Summary struct {
	Results            []TaskResult
	TotalBaseline      int
	TotalOptimized     int
	FailedTasks        []string // task IDs with one or more contract violations
	RoutedT1Count      int      // tasks whose Description routes to a T1 (cheap-model) agent
	SystemPromptTokens int      // measured size of the real embedded coder system prompt
}

// SavingsRatio is the fraction of aggregate baseline tokens the optimized
// path avoided across the whole suite.
func (s Summary) SavingsRatio() float64 {
	if s.TotalBaseline == 0 {
		return 0
	}
	return 1 - float64(s.TotalOptimized)/float64(s.TotalBaseline)
}

// coderSystemPrompt loads the embedded coder agent's system_prompt field, for
// the system-prompt-size check (PRD P1-005/G-012's "<800 tokens" target).
func coderSystemPrompt() (string, error) {
	data, err := defaults.ReadFile("agents/coder.yaml")
	if err != nil {
		return "", fmt.Errorf("eval: load embedded agents/coder.yaml: %w", err)
	}
	var cfg struct {
		SystemPrompt string `yaml:"system_prompt"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("eval: parse embedded agents/coder.yaml: %w", err)
	}
	return cfg.SystemPrompt, nil
}

// Summarize aggregates results into a Summary, additionally measuring the
// real embedded coder system prompt's token count.
func Summarize(results []TaskResult) (Summary, error) {
	prompt, err := coderSystemPrompt()
	if err != nil {
		return Summary{}, err
	}
	promptTokens := model.NewTokenCounter(evalModelName).CountString(prompt)

	s := Summary{Results: results, SystemPromptTokens: promptTokens}
	for _, r := range results {
		s.TotalBaseline += r.BaselineTokens
		s.TotalOptimized += r.OptimizedTokens
		if !r.Success() {
			s.FailedTasks = append(s.FailedTasks, r.TaskID)
		}
		if r.RoutedTier == "T1" {
			s.RoutedT1Count++
		}
	}
	return s, nil
}

// RenderMarkdown renders a human-readable dashboard (PRD P3-006's "publish
// results in a dashboard").
func (s Summary) RenderMarkdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Chronos Code — Token Efficiency Eval Suite\n\n")
	fmt.Fprintf(&b, "> **Synthetic fixture replay:** These results measure deterministic local tool fixtures, not paired Chronos Code and external baseline-tool model runs. They are not a valid external performance benchmark.\n\n")
	fmt.Fprintf(&b, "%d tasks, %d failed contract checks.\n\n", len(s.Results), len(s.FailedTasks))
	fmt.Fprintf(&b, "- **Aggregate savings**: %.1f%% (%d baseline tokens -> %d optimized tokens)\n", s.SavingsRatio()*100, s.TotalBaseline, s.TotalOptimized)
	fmt.Fprintf(&b, "- **System prompt size**: %d tokens (target <%d)\n", s.SystemPromptTokens, systemPromptTokenBudget)
	fmt.Fprintf(&b, "- **Routed to a T1 (cheap-model) agent**: %d/%d tasks\n\n", s.RoutedT1Count, len(s.Results))

	fmt.Fprintf(&b, "| Task | Category | Difficulty | Baseline | Optimized | Savings | Route | Status |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|\n")
	for _, r := range s.Results {
		status := "OK"
		if !r.Success() {
			status = fmt.Sprintf("FAIL (%d)", len(r.Violations))
		}
		route := r.RoutedAgent
		if route == "" {
			route = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %.1f%% | %s | %s |\n",
			r.TaskID, r.Category, r.Difficulty, r.BaselineTokens, r.OptimizedTokens, r.SavingsRatio()*100, route, status)
	}

	if len(s.FailedTasks) > 0 {
		fmt.Fprintf(&b, "\n## Violations\n\n")
		for _, r := range s.Results {
			if r.Success() {
				continue
			}
			fmt.Fprintf(&b, "- **%s**:\n", r.TaskID)
			for _, v := range r.Violations {
				fmt.Fprintf(&b, "  - %s\n", v)
			}
		}
	}

	return b.String()
}
