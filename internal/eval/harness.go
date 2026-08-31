package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage/adapters/memory"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/toolcompress"
)

// evalModelName pins the tokenizer used for every token count in this
// package, so results are stable across runs regardless of what the
// caller's own config.yaml selects as its active model.
const evalModelName = "claude-sonnet-4-6"

// outlineThresholdBytes mirrors internal/incctx's unexported
// outlineSizeThreshold, letting RunTask predict whether a given file_read
// should trigger P2-007's outline path.
const outlineThresholdBytes = 2000

// compressionThresholdTokens mirrors toolcompress.DefaultThresholdTokens,
// used at a fixed (non-budget-ramped) value so results are deterministic.
const compressionThresholdTokens = toolcompress.DefaultThresholdTokens

// stubProvider is a model.Provider that is never actually invoked. RunTask
// needs a.Model.Model() only so toolcompress.WrapDynamic can pick a
// tokenizer; Chat/StreamChat exist solely to satisfy the interface.
type stubProvider struct{ modelName string }

func (s stubProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, fmt.Errorf("eval: stub provider does not serve chat requests")
}

func (s stubProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, fmt.Errorf("eval: stub provider does not serve chat requests")
}

func (s stubProvider) Name() string  { return "eval-stub" }
func (s stubProvider) Model() string { return s.modelName }

// TaskResult is the outcome of replaying one Task's Steps through both the
// baseline and optimized tool registries.
type TaskResult struct {
	TaskID          string
	Category        Category
	Difficulty      Difficulty
	BaselineTokens  int
	OptimizedTokens int
	RoutedAgent     string   // informational: which agent routing.yaml would send Description to
	RoutedTier      string   // informational: that agent's configured tier (T1/T2)
	Violations      []string // non-empty means a P1-006/P2-007/P2-008 contract didn't fire as expected
}

// Success reports whether every efficiency contract this task exercises
// actually fired.
func (r TaskResult) Success() bool { return len(r.Violations) == 0 }

// SavingsRatio is the fraction of baseline tokens the optimized path avoided.
func (r TaskResult) SavingsRatio() float64 {
	if r.BaselineTokens == 0 {
		return 0
	}
	return 1 - float64(r.OptimizedTokens)/float64(r.BaselineTokens)
}

// newRegistry builds a tool registry with chronos's real builtin file tools
// (rooted at dir) plus a synthetic "shell" tool. Both the baseline and
// optimized registries are built this way so they start from an identical,
// unwrapped state.
func newRegistry(dir string, difficulty Difficulty) *tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(builtins.NewFileReadTool(dir))
	reg.Register(builtins.NewFileWriteTool(dir))
	reg.Register(builtins.NewFileListTool(dir))
	reg.Register(builtins.NewFileGlobTool(dir))
	reg.Register(builtins.NewFileGrepTool(dir))
	reg.Register(shellTool(difficulty))
	return reg
}

// shellTool returns a deterministic synthetic "shell" tool standing in for
// running the fixture's tests. There is no real Go module to `go test` in a
// generated fixture, and real test execution would make the suite flaky and
// slow; the output size scales with Difficulty, mirroring the PRD's own
// worked example (a 200-line `go test ./...` run compressed by P1-006).
func shellTool(d Difficulty) *tool.Definition {
	lineCounts := map[Difficulty]int{DifficultyEasy: 80, DifficultyMedium: 150, DifficultyHard: 250}
	lines := lineCounts[d]
	return &tool.Definition{
		Name:        "shell",
		Description: "Run a shell command (simulated in the eval harness).",
		Permission:  tool.PermRequireApproval,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd": map[string]any{"type": "string"},
			},
			"required": []string{"cmd"},
		},
		Handler: func(context.Context, map[string]any) (any, error) {
			var b strings.Builder
			for i := 0; i < lines; i++ {
				fmt.Fprintf(&b, "PASS: TestCase%d (0.00s)\n", i)
			}
			b.WriteString("ok  \tfixture/pkg\t0.010s\n")
			return map[string]any{"output": b.String(), "exit_code": 0}, nil
		},
	}
}

// buildRouter loads chronos-code's embedded routing.yaml and returns the
// deterministic T0 router plus a map from intent name to configured tier, for
// TaskResult's informational RoutedAgent/RoutedTier fields.
func buildRouter() (*router.Router, map[string]string, error) {
	data, err := defaults.ReadFile("routing.yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("eval: load embedded routing.yaml: %w", err)
	}
	cfg, err := router.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("eval: parse routing.yaml: %w", err)
	}
	rt, err := router.New(cfg, "coder")
	if err != nil {
		return nil, nil, fmt.Errorf("eval: build router: %w", err)
	}
	tiers := make(map[string]string, len(cfg.IntentRouting))
	for _, ir := range cfg.IntentRouting {
		tiers[ir.Intent] = ir.Tier
	}
	return rt, tiers, nil
}

// RunTask replays t.Steps through a fresh baseline and optimized registry
// pair, materialized in a temp workspace. No LLM is called.
func RunTask(ctx context.Context, t Task, rt *router.Router, tiers map[string]string) (TaskResult, error) {
	dir, err := os.MkdirTemp("", "chronos-eval-*")
	if err != nil {
		return TaskResult{}, fmt.Errorf("eval: task %s: create workspace: %w", t.ID, err)
	}
	defer os.RemoveAll(dir)

	sizes := make(map[string]int, len(t.Files))
	for relPath, content := range t.Files {
		sizes[relPath] = len(content)
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: write fixture %s: %w", t.ID, relPath, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: write fixture %s: %w", t.ID, relPath, err)
		}
	}

	baselineReg := newRegistry(dir, t.Difficulty)

	optAgent := &agent.Agent{
		ID:      t.ID + "-optimized",
		Tools:   newRegistry(dir, t.Difficulty),
		Storage: memory.New(),
		Model:   stubProvider{evalModelName},
	}
	// Wrap in the same order internal/orchestrator.New uses: toolcompress
	// wraps the raw handlers first, then incctx wraps file_read again on top
	// — so a first-time large-file read short-circuits at the incctx layer
	// (outline) before ever reaching the toolcompress layer beneath it,
	// exactly like a real agent's tool call.
	toolcompress.WrapDynamic(optAgent, func(context.Context) int { return compressionThresholdTokens })
	incctx.Wrap(optAgent, dir)

	counter := model.NewTokenCounter(evalModelName)
	res := TaskResult{TaskID: t.ID, Category: t.Category, Difficulty: t.Difficulty}
	seen := make(map[string]bool)

	for _, step := range t.Steps {
		baseDef, ok := baselineReg.Get(step.Tool)
		if !ok {
			return TaskResult{}, fmt.Errorf("eval: task %s: unknown baseline tool %q", t.ID, step.Tool)
		}
		optDef, ok := optAgent.Tools.Get(step.Tool)
		if !ok {
			return TaskResult{}, fmt.Errorf("eval: task %s: unknown optimized tool %q", t.ID, step.Tool)
		}

		baseOut, err := baseDef.Handler(ctx, cloneArgs(step.Args))
		if err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: baseline %s: %w", t.ID, step.Tool, err)
		}
		optOut, err := optDef.Handler(ctx, cloneArgs(step.Args))
		if err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: optimized %s: %w", t.ID, step.Tool, err)
		}

		baseTokens := jsonTokens(counter, baseOut)
		res.BaselineTokens += baseTokens
		res.OptimizedTokens += jsonTokens(counter, optOut)
		res.Violations = append(res.Violations, checkContract(step, optOut, baseTokens, seen, sizes)...)

		if step.Tool == "file_read" {
			if path, _ := step.Args["path"].(string); path != "" {
				seen[path] = true
			}
		}
	}

	if intent, agentID, matched := rt.Classify(t.Description); matched {
		res.RoutedAgent = agentID
		res.RoutedTier = tiers[intent]
	}

	return res, nil
}

// jsonTokens returns the tokenized size of v's JSON encoding, matching how
// toolcompress measures a tool result (encoding/json marshal, then count).
func jsonTokens(counter model.TokenCounter, v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return counter.CountString(string(data))
}

// checkContract verifies that the optimized path's efficiency machinery
// fired when it should have, generalizing acceptance criteria from P1-006
// (compress large results), P2-007 (outline large Go files on first read,
// never outline on explicit-range/small reads), and P2-008 (skip redundant
// reads of an unchanged file).
func checkContract(step Step, optOut any, baselineTokens int, seen map[string]bool, sizes map[string]int) []string {
	out, _ := optOut.(map[string]any)

	if step.Tool == "file_read" {
		path, _ := step.Args["path"].(string)

		if seen[path] {
			if unchanged, _ := out["unchanged"].(bool); !unchanged {
				return []string{fmt.Sprintf("file_read: expected unchanged=true on repeat read of %s (P2-008)", path)}
			}
			return nil
		}

		_, hasStart := step.Args["start_line"]
		_, hasEnd := step.Args["end_line"]
		outlineOnly, hasOutlineOnly := step.Args["outline_only"].(bool)
		explicitFull := hasOutlineOnly && !outlineOnly
		shouldOutline := strings.HasSuffix(path, ".go") && !hasStart && !hasEnd && !explicitFull && sizes[path] > outlineThresholdBytes

		if shouldOutline {
			if outline, _ := out["outline"].(bool); !outline {
				return []string{fmt.Sprintf("file_read: expected outline=true for first read of %s (%d bytes) (P2-007)", path, sizes[path])}
			}
			return nil // outlining short-circuits before the inner toolcompress layer runs
		}
		if outline, _ := out["outline"].(bool); outline {
			return []string{fmt.Sprintf("file_read: unexpected outline=true for %s (explicit range or small file) (P2-007)", path)}
		}
		// Falls through: a small first-time read or an explicit-range/force
		// read reaches the inner toolcompress-wrapped handler like any other
		// tool result, so the generic compression check below still applies.
	}

	if baselineTokens > compressionThresholdTokens {
		if compressed, _ := out["compressed"].(bool); !compressed {
			return []string{fmt.Sprintf("%s: expected compressed=true (%d tokens > %d) (P1-006)", step.Tool, baselineTokens, compressionThresholdTokens)}
		}
	}
	return nil
}

// RunAll replays every Corpus task and returns their results in Corpus order.
func RunAll(ctx context.Context) ([]TaskResult, error) {
	rt, tiers, err := buildRouter()
	if err != nil {
		return nil, err
	}
	corpus := Corpus()
	results := make([]TaskResult, 0, len(corpus))
	for _, t := range corpus {
		res, err := RunTask(ctx, t, rt, tiers)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}
