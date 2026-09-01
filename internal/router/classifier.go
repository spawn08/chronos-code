package router

import (
	"regexp"
	"strings"
)

type Complexity string

const (
	ComplexityLow    Complexity = "low"
	ComplexityMedium Complexity = "medium"
	ComplexityHigh   Complexity = "high"
)

type TaskKind string

const (
	TaskKindEdit      TaskKind = "edit"
	TaskKindRefactor  TaskKind = "refactor"
	TaskKindDebug     TaskKind = "debug"
	TaskKindArchitect TaskKind = "architect"
	TaskKindExplain   TaskKind = "explain"
)

type Classification struct {
	Complexity Complexity
	Kind       TaskKind
}

var fileReferencePattern = regexp.MustCompile(`\b[[:alnum:]_./-]+\.[[:alpha:]]{1,8}\b`)

func ClassifyTask(message string) Classification {
	text := strings.ToLower(strings.TrimSpace(message))
	kind := TaskKindEdit
	switch {
	case containsAny(text, "refactor", "extract", "rename", "move "):
		kind = TaskKindRefactor
	case containsAny(text, "fix", "bug", "error", "crash", "panic", "failing"):
		kind = TaskKindDebug
	case containsAny(text, "explain", "what is", "how does", "why does"):
		kind = TaskKindExplain
	case containsAny(text, "design", "architect", "plan ", "roadmap"):
		kind = TaskKindArchitect
	}
	complexity := ComplexityLow
	if len(text) > 500 || len(fileReferencePattern.FindAllString(text, -1)) >= 3 || containsAny(text, "across", "entire", "all ", "every", "multi", "multiple") {
		complexity = ComplexityHigh
	} else if kind == TaskKindDebug {
		complexity = ComplexityMedium
	}
	return Classification{Complexity: complexity, Kind: kind}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}
