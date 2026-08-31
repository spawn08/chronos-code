// Package eval implements PRD P3-006: a reproducible token-efficiency eval
// suite with a CI regression gate. Each Task replays a fixed reference
// trajectory of tool calls (the sequence a competent coding agent would make
// to complete Description) through two identically-configured tool
// registries: "baseline" (raw, unwrapped chronos builtins) and "optimized"
// (wrapped exactly as internal/orchestrator.New wires a real agent — same
// order, same wrappers: internal/toolcompress then internal/incctx). No LLM
// is called: replaying a fixed trajectory isolates what the tool-layer
// efficiency machinery (P1-006, P2-007, P2-008) saves, independent of any
// particular model's behavior, so the suite is free, fast, and fully
// deterministic — a hard requirement for a gate that runs on every PR.
package eval

import (
	"fmt"
	"strings"
)

// Category is the kind of coding task, per PRD P3-006's "bug fixes,
// features, refactors" corpus requirement.
type Category string

const (
	CategoryBugfix   Category = "bugfix"
	CategoryFeature  Category = "feature"
	CategoryRefactor Category = "refactor"
)

// Difficulty is the task's complexity tier, per PRD P3-006's "easy/medium/
// hard" corpus requirement.
type Difficulty string

const (
	DifficultyEasy   Difficulty = "easy"
	DifficultyMedium Difficulty = "medium"
	DifficultyHard   Difficulty = "hard"
)

// Step is one tool call in a task's reference trajectory.
type Step struct {
	Tool string
	Args map[string]any
}

// Task is one entry in the eval suite's corpus.
type Task struct {
	ID          string
	Category    Category
	Difficulty  Difficulty
	Description string            // the user request; fed to the router for informational tier classification only
	Files       map[string]string // relative path -> content, materialized into a temp workspace per run
	Steps       []Step            // reference trajectory replayed against both registries
}

// cloneArgs returns a shallow copy of a, so the same Step can be replayed
// against the baseline and optimized registries without one call's argument
// mutations (e.g. incctx.Wrap deletes "force") leaking into the other.
func cloneArgs(a map[string]any) map[string]any {
	c := make(map[string]any, len(a))
	for k, v := range a {
		c[k] = v
	}
	return c
}

// genGoFile deterministically generates a syntactically valid Go source file
// with n top-level functions named prefix0..prefixN-1. It exists to produce
// fixtures of a controlled size: at ~190 bytes/function, n=4-8 stays well
// under incctx's 2000-byte outline threshold (small/first-read cases) while
// n=15+ comfortably exceeds it (outline cases), giving each Task a
// predictable P2-007 contract to check without hand-authoring file content.
func genGoFile(pkg string, n int, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "// %s%d computes a derived value from x for case %d.\n", prefix, i, i)
		fmt.Fprintf(&b, "func %s%d(x int) int {\n", prefix, i)
		fmt.Fprintf(&b, "\ty := x*%d + %d\n", i+1, i)
		fmt.Fprintf(&b, "\tif y > 1000 {\n\t\ty = y %% 1000\n\t}\n")
		fmt.Fprintf(&b, "\treturn y\n}\n\n")
	}
	return b.String()
}

// categoryVerb renders a Description prefix that reads like a real user
// request for the given Category.
func categoryVerb(c Category) string {
	switch c {
	case CategoryBugfix:
		return "Fix a bug in"
	case CategoryFeature:
		return "Add a new capability to"
	default:
		return "Refactor"
	}
}

// buildTask constructs one corpus entry for the given category/difficulty/
// variant. Difficulty determines file count and size (and therefore which
// P2-007/P2-008 contracts the task exercises); variant only varies naming so
// tasks in the same category/difficulty cell are distinct, not duplicates.
func buildTask(category Category, difficulty Difficulty, variant int) Task {
	id := fmt.Sprintf("%s-%s-%d", category, difficulty, variant)
	verb := categoryVerb(category)
	prefix := fmt.Sprintf("Op%d", variant)

	switch difficulty {
	case DifficultyEasy:
		symbol := fmt.Sprintf("%s3", prefix)
		return Task{
			ID:          id,
			Category:    category,
			Difficulty:  difficulty,
			Description: fmt.Sprintf("%s the %s logic in handler.go", verb, symbol),
			Files: map[string]string{
				"handler.go": genGoFile("handler", 8, prefix),
			},
			Steps: []Step{
				{Tool: "file_list", Args: map[string]any{"path": "."}},
				{Tool: "file_read", Args: map[string]any{"path": "handler.go"}},
				{Tool: "file_grep", Args: map[string]any{"path": "handler.go", "pattern": symbol}},
				{Tool: "file_read", Args: map[string]any{"path": "handler.go"}},
				{Tool: "shell", Args: map[string]any{"cmd": "go test ./..."}},
			},
		}

	case DifficultyMedium:
		symbol := fmt.Sprintf("%s5", prefix)
		return Task{
			ID:          id,
			Category:    category,
			Difficulty:  difficulty,
			Description: fmt.Sprintf("%s the %s method in service.go", verb, symbol),
			Files: map[string]string{
				"types.go":   genGoFile("types", 4, prefix+"T"),
				"service.go": genGoFile("service", 15, prefix),
			},
			Steps: []Step{
				{Tool: "file_list", Args: map[string]any{"path": "."}},
				{Tool: "file_read", Args: map[string]any{"path": "types.go"}},
				{Tool: "file_read", Args: map[string]any{"path": "service.go"}},
				{Tool: "file_grep", Args: map[string]any{"path": "service.go", "pattern": symbol}},
				{Tool: "file_read", Args: map[string]any{"path": "service.go"}},
				{Tool: "shell", Args: map[string]any{"cmd": "go test ./..."}},
			},
		}

	default: // DifficultyHard
		symbol := fmt.Sprintf("%s10", prefix)
		return Task{
			ID:          id,
			Category:    category,
			Difficulty:  difficulty,
			Description: fmt.Sprintf("%s the %s flow spanning engine.go and adapter.go", verb, symbol),
			Files: map[string]string{
				"config.small.go": genGoFile("config", 4, prefix+"C"),
				"engine.go":       genGoFile("engine", 25, prefix),
				"adapter.go":      genGoFile("adapter", 20, prefix+"A"),
			},
			Steps: []Step{
				{Tool: "file_list", Args: map[string]any{"path": "."}},
				{Tool: "file_read", Args: map[string]any{"path": "config.small.go"}},
				{Tool: "file_read", Args: map[string]any{"path": "engine.go"}},
				{Tool: "file_read", Args: map[string]any{"path": "adapter.go", "start_line": 1, "end_line": 20}},
				{Tool: "file_grep", Args: map[string]any{"path": "engine.go", "pattern": symbol}},
				{Tool: "file_read", Args: map[string]any{"path": "engine.go"}},
				{Tool: "shell", Args: map[string]any{"cmd": "go test ./... -run " + symbol}},
			},
		}
	}
}

// Corpus returns the eval suite's task set: 3 categories x 3 difficulties x
// 3 variants = 27 tasks, within PRD P3-006's 20-30 target.
func Corpus() []Task {
	categories := []Category{CategoryBugfix, CategoryFeature, CategoryRefactor}
	difficulties := []Difficulty{DifficultyEasy, DifficultyMedium, DifficultyHard}

	tasks := make([]Task, 0, len(categories)*len(difficulties)*3)
	for _, c := range categories {
		for _, d := range difficulties {
			for v := 1; v <= 3; v++ {
				tasks = append(tasks, buildTask(c, d, v))
			}
		}
	}
	return tasks
}
