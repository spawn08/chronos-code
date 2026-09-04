// Package router implements the deterministic, zero-LLM-cost (T0) intent
// classification half of PRD item P2-006 "Tiered model routing". It parses
// the intent_routing patterns curated in internal/defaults/routing.yaml and
// matches incoming user messages against them using compiled regexes, before
// any LLM-based (T1) classification is attempted.
package router

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// defaultPatternPlaceholder is the literal, documentation-only pattern string
// used in routing.yaml for the "code" intent. It is not a real regex trigger
// and must never be compiled — the "code" intent's real job is served by the
// fallback return in Classify.
const defaultPatternPlaceholder = "default — all other requests"

// modelConfig describes the LLM used by the router itself (T1 classification
// step). It is not used by the deterministic T0 classifier in this package,
// but is kept so Parse doesn't silently drop it.
type modelConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// routerSection mirrors the `router:` top-level key in routing.yaml.
type routerSection struct {
	Model                modelConfig `yaml:"model"`
	BudgetTokens         int         `yaml:"budget_tokens"`
	ClassificationPrompt string      `yaml:"classification_prompt"`
}

// IntentRoute mirrors one entry of the `intent_routing:` list in routing.yaml.
type IntentRoute struct {
	Intent   string   `yaml:"intent"`
	Agent    string   `yaml:"agent"`
	Tier     string   `yaml:"tier"`
	Patterns []string `yaml:"patterns"`
	Reason   string   `yaml:"reason"`
}

// ModelSpec identifies a model and the provider used to construct it.
type ModelSpec struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// ModelRouting maps task complexity and kind to the model that should handle
// the task. Missing cells resolve to the medium/edit model.
type ModelRouting struct {
	Models map[Complexity]map[TaskKind]ModelSpec `yaml:"models"`
}

// Config is the subset of routing.yaml this package understands. The
// explicit_switch, escalation, pipelines, and cost_optimization sections are
// intentionally not modeled — yaml.Unmarshal drops unknown keys.
// ImplementationPath is the bounded execution graph for a complexity band.
type ImplementationPath struct {
	MaxToolCalls int    `yaml:"max_tool_calls"`
	Graph        string `yaml:"graph"`
	Plan         string `yaml:"plan"`
	Hint         string `yaml:"hint"`
}

// Config is the subset of routing.yaml this package understands. The
// explicit_switch, escalation, pipelines, and cost_optimization sections are
// intentionally not modeled — yaml.Unmarshal drops unknown keys.
type Config struct {
	Router              routerSection                     `yaml:"router"`
	IntentRouting       []IntentRoute                     `yaml:"intent_routing"`
	ModelRouting        ModelRouting                      `yaml:"model_routing"`
	ImplementationPaths map[Complexity]ImplementationPath `yaml:"implementation_paths"`
	PPD                 PPDConfig                         `yaml:"ppd"`
}

// Parse decodes raw YAML bytes (e.g. loaded via internal/defaults.ReadFile)
// into a Config. The caller is responsible for supplying the bytes; this
// package does not read files itself.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("router: parse routing config: %w", err)
	}
	return &cfg, nil
}

// ResolveModel returns the exact complexity/kind model when declared, falling
// back to medium/edit otherwise. It returns ok=false when neither cell exists,
// including for legacy routing YAML without a model_routing section.
func (c *Config) ResolveModel(complexity Complexity, kind TaskKind) (ModelSpec, bool) {
	if byKind, ok := c.ModelRouting.Models[complexity]; ok {
		if model, ok := byKind[kind]; ok {
			return model, true
		}
	}
	model, ok := c.ModelRouting.Models[ComplexityMedium][TaskKindEdit]
	return model, ok
}

// DefaultPath is the built-in execution graph when routing YAML omits
// implementation_paths for a complexity band.
func DefaultPath(complexity Complexity) ImplementationPath {
	switch complexity {
	case ComplexityHigh:
		return ImplementationPath{
			MaxToolCalls: 24,
			Graph:        "L0-L3",
			Plan:         "ppd-or-update_plan",
			Hint:         "L0 landscape; spawn ppd-planner if multi-package; leaf-first; verify each node; remember decisions",
		}
	case ComplexityMedium:
		return ImplementationPath{
			MaxToolCalls: 12,
			Graph:        "L0-L2",
			Plan:         "update_plan",
			Hint:         "recall learnings; impact_analysis before edits; test_map after; spawn only for an isolated loop",
		}
	default:
		return ImplementationPath{
			MaxToolCalls: 4,
			Graph:        "L2",
			Plan:         "skip",
			Hint:         "graph_query/resolve_symbol; ranged read if needed; one edit or answer; skip spawn",
		}
	}
}

// PathFor returns the configured path for complexity, falling back to DefaultPath.
func (c *Config) PathFor(complexity Complexity) ImplementationPath {
	if c != nil {
		if path, ok := c.ImplementationPaths[complexity]; ok && path.Hint != "" {
			if path.MaxToolCalls == 0 {
				path.MaxToolCalls = DefaultPath(complexity).MaxToolCalls
			}
			if path.Graph == "" {
				path.Graph = DefaultPath(complexity).Graph
			}
			if path.Plan == "" {
				path.Plan = DefaultPath(complexity).Plan
			}
			return path
		}
	}
	return DefaultPath(complexity)
}

// compiledRoute is an intent route with its patterns pre-compiled as
// case-insensitive regexes.
type compiledRoute struct {
	intent   string
	agent    string
	patterns []*regexp.Regexp
}

// Router performs deterministic (T0) intent classification by matching a
// user message against the ordered list of compiled routes. When a T1
// fallback classifier is attached (see SetT1), unmatched messages can also be
// classified by a cheap model instead of always defaulting to defaultAgent.
type Router struct {
	routes       []compiledRoute
	defaultAgent string
	intentAgents map[string]string
	t1           Classifier
}

// New builds a Router from cfg, preserving the declaration order of
// cfg.IntentRouting (this matters for tie-break priority: earlier routes win
// over later ones, e.g. the generic "code" fallback should be declared last).
// defaultAgent is returned by Classify when no pattern matches.
func New(cfg *Config, defaultAgent string) (*Router, error) {
	routes := make([]compiledRoute, 0, len(cfg.IntentRouting))
	intentAgents := make(map[string]string, len(cfg.IntentRouting))
	for _, ir := range cfg.IntentRouting {
		cr := compiledRoute{intent: ir.Intent, agent: ir.Agent}
		for _, p := range ir.Patterns {
			if p == defaultPatternPlaceholder {
				continue
			}
			re, err := regexp.Compile("(?i)" + p)
			if err != nil {
				return nil, fmt.Errorf("router: compile pattern %q for intent %q: %w", p, ir.Intent, err)
			}
			cr.patterns = append(cr.patterns, re)
		}
		routes = append(routes, cr)
		intentAgents[ir.Intent] = ir.Agent
	}
	return &Router{routes: routes, defaultAgent: defaultAgent, intentAgents: intentAgents}, nil
}

// Classify matches message against the compiled routes in declaration order,
// returning the first matching intent/agent pair. If no route matches, it
// returns the generic "code" intent and the configured default agent, with
// matched set to false.
func (r *Router) Classify(message string) (intent, agentID string, matched bool) {
	for _, route := range r.routes {
		for _, re := range route.patterns {
			if re.MatchString(message) {
				return route.intent, route.agent, true
			}
		}
	}
	return "code", r.defaultAgent, false
}
