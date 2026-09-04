package router

import (
	"testing"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/modelinfo"
)

// routingYAML is the legacy routing config from before model_routing was
// added, retained to verify compatibility with existing user overrides.
const routingYAML = `
# Chronos Code — Agent Routing Configuration
# Routes user messages to the right agent based on intent classification.

meta:
  version: "1.0.0"
  description: "Intent-based message routing across chronos-code agents"

# Router configuration
router:
  description: |
    The router is the first thing that runs on every user message. It
    classifies intent and selects the cheapest agent that can handle it.
    The router itself uses a T1 (cheap) model with a tight token budget.
  model:
    provider: "anthropic"
    model: "claude-haiku-4-5"
  budget_tokens: 500
  classification_prompt: |
    Classify this user message into ONE intent. Reply with only the intent name.
    Intents: explain, review, debug, plan, search, architecture, code (default).

    Rules:
    - "explain" = user wants something explained, not changed
    - "review" = user wants existing code reviewed/audited
    - "debug" = user mentions an error, failure, crash, or test failing
    - "plan" = user wants to decompose or plan before implementing
    - "search" = user wants to find something (file, function, pattern)
    - "architecture" = user asks about design, structure, patterns
    - "code" = everything else (implement, fix, refactor, add, change, write)

# Intent → Agent mapping
intent_routing:

  - intent: "explain"
    agent: "explainer"
    tier: "T1"
    patterns:
      - "explain"
      - "what does"
      - "how does"
      - "why does"
      - "walk me through"
      - "what is"
      - "how is.*used"
      - "describe"
      - "tell me about"
    reason: "Explanation is read + summarize — cheap model handles it"

  - intent: "review"
    agent: "reviewer"
    tier: "T2"
    patterns:
      - "review"
      - "check (my|the|this)"
      - "audit"
      - "look at (my|the|this) (changes|diff|code|PR)"
      - "any (bugs|issues|problems)"
      - "code quality"
      - "security (check|audit|review)"
    reason: "Review needs nuanced judgment and security awareness"

  - intent: "debug"
    agent: "debugger"
    tier: "T2"
    patterns:
      - "debug"
      - "fix (the|this) (error|failure|crash|bug|issue)"
      - "why is.*failing"
      - "test.*fail"
      - "broken"
      - "doesn't work"
      - "stack trace"
      - "panic"
      - "segfault"
      - "nil pointer"
    reason: "Diagnosis needs deep reasoning and error trace analysis"

  - intent: "plan"
    agent: "planner"
    tier: "T2"
    patterns:
      - "plan"
      - "break down"
      - "decompose"
      - "how should I approach"
      - "what steps"
      - "strategy for"
      - "design a solution"
      - "roadmap"
    reason: "Planning needs strong reasoning to decompose correctly"

  - intent: "search"
    agent: "researcher"
    tier: "T1"
    patterns:
      - "find"
      - "where is"
      - "which file"
      - "search"
      - "locate"
      - "grep"
      - "show me"
      - "list all"
      - "who (calls|uses|implements)"
    reason: "Search is mechanical — graph queries + cheap model"

  - intent: "architecture"
    agent: "architect"
    tier: "T2"
    patterns:
      - "design"
      - "architecture"
      - "structure"
      - "should I use.*pattern"
      - "dependency"
      - "package (layout|structure|organization)"
      - "interface design"
      - "abstraction"
    reason: "Design needs broad knowledge and careful judgment"

  - intent: "code"
    agent: "coder"
    tier: "T2"
    patterns:
      - "default — all other requests"
    reason: "General coding tasks go to the primary agent"

# Explicit agent switch
explicit_switch:
  syntax: "@<agent_id> <message>"
  description: |
    Users can bypass the router and address a specific agent directly.
    The @ prefix routes to the named agent regardless of intent classification.
  examples:
    - "@reviewer check my last commit for security issues"
    - "@planner break down this feature into tasks"
    - "@debugger why is TestBuildAgent failing with nil pointer"
    - "@explainer how does the middleware chain work in this project"
    - "@researcher find all files that import the storage package"
    - "@architect should we use the repository pattern here"
    - "@coder add error handling to the HTTP handler"

# Auto-escalation rules
escalation:
  description: |
    When a cheaper agent can't complete the task, it escalates to a
    more capable agent. Escalation is automatic and transparent.
  rules:
    - from: "researcher"
      to: "coder"
      trigger: "Researcher determines the task requires code changes, not just search"
      example: "User asks 'find and fix all TODO comments' — researcher finds them, coder fixes them"

    - from: "explainer"
      to: "coder"
      trigger: "User follow-up implies they want changes, not just explanation"
      example: "After explanation, user says 'ok, refactor it' — escalate to coder"

    - from: "planner"
      to: "coder"
      trigger: "Plan is approved (explicitly or implicitly) and user wants execution"
      example: "Planner outputs plan, user says 'go ahead' or 'looks good, do it'"

    - from: "debugger"
      to: "coder"
      trigger: "Debugger identifies the fix but needs to apply it"
      example: "Debugger traces root cause, coder applies the fix and runs tests"

# Multi-agent pipelines
pipelines:
  description: |
    For complex tasks, chronos-code can run multi-agent pipelines where
    agents work sequentially or in coordination. Pipelines are triggered
    by task complexity (determined by the router) or explicitly by the user.

  definitions:
    - id: "code-review-pipeline"
      name: "Code Review Pipeline"
      strategy: "sequential"
      trigger: "User asks for a thorough code review with fixes"
      agents: ["planner", "coder", "reviewer"]
      description: |
        1. Planner decomposes the review into areas of concern
        2. Coder implements any suggested fixes
        3. Reviewer verifies the fixes don't introduce new issues

    - id: "debug-pipeline"
      name: "Debug Pipeline"
      strategy: "coordinator"
      trigger: "Complex bug spanning multiple files or systems"
      coordinator: "debugger"
      agents: ["debugger", "researcher", "coder"]
      max_iterations: 5
      description: |
        Debugger coordinates: sends researcher to gather context,
        identifies root cause, sends coder to apply fix, then verifies.

    - id: "feature-pipeline"
      name: "Feature Implementation Pipeline"
      strategy: "sequential"
      trigger: "User requests a new feature (detected by router)"
      agents: ["planner", "coder", "reviewer"]
      description: |
        1. Planner decomposes the feature into implementation steps
        2. Coder implements each step with tests
        3. Reviewer checks the implementation for correctness and style

    - id: "migration-pipeline"
      name: "Migration Pipeline"
      strategy: "sequential"
      trigger: "User requests a dependency or API migration"
      agents: ["researcher", "planner", "coder", "reviewer"]
      description: |
        1. Researcher inventories all usage sites of the migrated API
        2. Planner creates ordered migration steps
        3. Coder applies changes file-by-file with tests
        4. Reviewer verifies no regressions

# Cost optimization rules
cost_optimization:
  description: |
    Routing always prefers the cheapest agent that can handle the task.
    These rules encode the cost hierarchy and fallback behavior.
  rules:
    - rule: "Try T0 (graph/file tools) before T1 (cheap model) before T2 (frontier model)"
    - rule: "If the task can be answered by a graph query alone, skip the LLM entirely"
    - rule: "Researchers and explainers use T1 models — don't waste frontier tokens on summarization"
    - rule: "Router misclassification penalty is low (worst case: user gets a slightly less capable agent that escalates)"
    - rule: "When uncertain, classify as 'code' — the coder can handle anything, it's just more expensive"
    - rule: "Pipeline routing adds latency — only trigger for tasks the router estimates need >5 turns"

  tier_costs:
    T0: "0 tokens (graph query, file operation)"
    T1: "~$0.25-1.00 / million tokens (Haiku-class)"
    T2: "~$3-15 / million tokens (Sonnet/Opus-class)"
    router: "~200-500 tokens per classification (~$0.0001)"
`

func TestParseAndClassify(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Router.BudgetTokens != 500 {
		t.Fatalf("Router.BudgetTokens = %d, want 500", cfg.Router.BudgetTokens)
	}
	if len(cfg.IntentRouting) != 7 {
		t.Fatalf("len(IntentRouting) = %d, want 7", len(cfg.IntentRouting))
	}
	if _, ok := cfg.ResolveModel(ComplexityLow, TaskKindEdit); ok {
		t.Fatal("ResolveModel() ok = true for legacy YAML without model_routing")
	}

	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cases := []struct {
		name       string
		message    string
		wantIntent string
		wantAgent  string
		wantMatch  bool
	}{
		{
			name:       "explain",
			message:    "please explain how the middleware chain works",
			wantIntent: "explain",
			wantAgent:  "explainer",
			wantMatch:  true,
		},
		{
			name:       "debug",
			message:    "fix the nil pointer panic in the parser",
			wantIntent: "debug",
			wantAgent:  "debugger",
			wantMatch:  true,
		},
		{
			name:       "search",
			message:    "find all callers of BuildAll",
			wantIntent: "search",
			wantAgent:  "researcher",
			wantMatch:  true,
		},
		{
			name:       "architecture",
			message:    "should I use the repository pattern here",
			wantIntent: "architecture",
			wantAgent:  "architect",
			wantMatch:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent, agent, matched := r.Classify(tc.message)
			if intent != tc.wantIntent || agent != tc.wantAgent || matched != tc.wantMatch {
				t.Errorf("Classify(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.message, intent, agent, matched, tc.wantIntent, tc.wantAgent, tc.wantMatch)
			}
		})
	}

	t.Run("code fallback", func(t *testing.T) {
		_, agent, _ := r.Classify("add a brand new CLI flag for output format")
		if agent != "coder" {
			t.Errorf("Classify() agent = %q, want %q", agent, "coder")
		}
	})
}

func TestBundledCodeIntentStaysOnChronosCode(t *testing.T) {
	data, err := defaults.ReadFile("routing.yaml")
	if err != nil {
		t.Fatalf("defaults.ReadFile(routing.yaml) error = %v", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	var codeAgent string
	for _, route := range cfg.IntentRouting {
		if route.Intent == "code" {
			codeAgent = route.Agent
		}
	}
	if codeAgent != "chronos-code" {
		t.Fatalf("bundled code intent agent = %q, want chronos-code", codeAgent)
	}
	r, err := New(cfg, "chronos-code")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, agent, matched := r.Classify("add a brand new CLI flag for output format")
	if matched || agent != "chronos-code" {
		t.Fatalf("Classify() = (%q, %v), want unmatched chronos-code", agent, matched)
	}
}

func TestBundledImplementationPaths(t *testing.T) {
	data, err := defaults.ReadFile("routing.yaml")
	if err != nil {
		t.Fatalf("defaults.ReadFile(routing.yaml) error = %v", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	low := cfg.PathFor(ComplexityLow)
	if low.MaxToolCalls != 4 || low.Plan != "skip" || low.Graph != "L2" {
		t.Fatalf("low path = %+v", low)
	}
	medium := cfg.PathFor(ComplexityMedium)
	if medium.MaxToolCalls != 12 || medium.Plan != "update_plan" {
		t.Fatalf("medium path = %+v", medium)
	}
	high := cfg.PathFor(ComplexityHigh)
	if high.MaxToolCalls != 24 || high.Plan != "ppd-or-update_plan" {
		t.Fatalf("high path = %+v", high)
	}
}

func TestBundledModelRouting(t *testing.T) {
	data, err := defaults.ReadFile("routing.yaml")
	if err != nil {
		t.Fatalf("defaults.ReadFile(routing.yaml) error = %v", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := map[Classification]ModelSpec{
		{Complexity: ComplexityLow, Kind: TaskKindEdit}:       {Provider: "anthropic", Model: "claude-haiku-4-5"},
		{Complexity: ComplexityLow, Kind: TaskKindExplain}:    {Provider: "anthropic", Model: "claude-haiku-4-5"},
		{Complexity: ComplexityMedium, Kind: TaskKindEdit}:    {Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Complexity: ComplexityHigh, Kind: TaskKindRefactor}:  {Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Complexity: ComplexityHigh, Kind: TaskKindDebug}:     {Provider: "anthropic", Model: "claude-opus-4-8"},
		{Complexity: ComplexityHigh, Kind: TaskKindArchitect}: {Provider: "anthropic", Model: "claude-opus-4-8"},
	}
	declared := 0
	for _, byKind := range cfg.ModelRouting.Models {
		declared += len(byKind)
	}
	if declared != len(want) {
		t.Fatalf("bundled model routing declares %d cells, want %d", declared, len(want))
	}
	for classification, wantSpec := range want {
		got, ok := cfg.ResolveModel(classification.Complexity, classification.Kind)
		if !ok || got != wantSpec {
			t.Errorf("ResolveModel(%q, %q) = (%+v, %v), want (%+v, true)", classification.Complexity, classification.Kind, got, ok, wantSpec)
		}
		if _, ok := modelinfo.Lookup(got.Provider, got.Model); !ok {
			t.Errorf("bundled model %+v is not registered in modelinfo", got)
		}
	}

	fallback := want[Classification{Complexity: ComplexityMedium, Kind: TaskKindEdit}]
	for i := 0; i < 2; i++ {
		got, ok := cfg.ResolveModel(ComplexityHigh, TaskKindExplain)
		if !ok || got != fallback {
			t.Errorf("fallback attempt %d = (%+v, %v), want (%+v, true)", i+1, got, ok, fallback)
		}
	}
}

func TestNew_InvalidRegexError(t *testing.T) {
	cfg := &Config{
		IntentRouting: []IntentRoute{
			{
				Intent:   "broken",
				Agent:    "whatever",
				Patterns: []string{"foo("},
			},
		},
	}
	if _, err := New(cfg, "coder"); err == nil {
		t.Fatal("New() error = nil, want error for invalid regex pattern")
	}
}

func TestNew_OrderingPriority(t *testing.T) {
	cfg := &Config{
		IntentRouting: []IntentRoute{
			{
				Intent:   "first",
				Agent:    "first-agent",
				Patterns: []string{"hello"},
			},
			{
				Intent:   "second",
				Agent:    "second-agent",
				Patterns: []string{"hello"},
			},
		},
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	intent, agent, matched := r.Classify("hello there")
	if intent != "first" || agent != "first-agent" || !matched {
		t.Errorf("Classify() = (%q, %q, %v), want (%q, %q, true) — first route should win",
			intent, agent, matched, "first", "first-agent")
	}
}
