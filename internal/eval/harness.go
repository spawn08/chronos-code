package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// workspacePlaceholder replaces each task's real temp workspace directory in
// every tool result (see wrapNormalize) so token counts and content hashes
// never depend on os.MkdirTemp's random suffix or the OS's temp dir prefix,
// both of which vary run-to-run and machine-to-machine.
const workspacePlaceholder = "/workspace"

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
	Violations      []string // missing requested evidence or incorrect final compression
}

// Success reports whether every step delivered its requested evidence and
// applied compression to the final logical result when needed.
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

// normalizeResult recursively rewrites any string in v that contains dir,
// replacing it with placeholder. It only needs to handle the concrete shapes
// chronos's builtin file tools and the synthetic shell tool actually return:
// map[string]any, []map[string]any, and string leaves.
func normalizeResult(v any, dir, placeholder string) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = normalizeResult(vv, dir, placeholder)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(val))
		for i, vv := range val {
			out[i], _ = normalizeResult(vv, dir, placeholder).(map[string]any)
		}
		return out
	case []string:
		out := make([]string, len(val))
		for i, s := range val {
			out[i] = strings.ReplaceAll(s, dir, placeholder)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, v := range val {
			out[i] = normalizeResult(v, dir, placeholder)
		}
		return out
	case string:
		return strings.ReplaceAll(val, dir, placeholder)
	default:
		return val
	}
}

// wrapNormalize runs after logical handlers (including incctx) and before
// compression/token counting. Normalize the actual returned representation so
// outlines, ranges, hashes and previews cannot contain random workspace paths.
func wrapNormalize(reg *tool.Registry, dir, placeholder string) {
	for _, def := range reg.List() {
		orig := def.Handler
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := orig(ctx, args)
			if err != nil {
				return result, err
			}
			return normalizeResult(result, dir, placeholder), nil
		}
	}
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

	for relPath, content := range t.Files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: write fixture %s: %w", t.ID, relPath, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return TaskResult{}, fmt.Errorf("eval: task %s: write fixture %s: %w", t.ID, relPath, err)
		}
	}

	baselineReg := newRegistry(dir, t.Difficulty)
	wrapNormalize(baselineReg, dir, workspacePlaceholder)

	optReg := newRegistry(dir, t.Difficulty)
	optAgent := &agent.Agent{
		ID:      t.ID + "-optimized",
		Tools:   optReg,
		Storage: memory.New(),
		Model:   stubProvider{evalModelName},
	}
	// Match the production logical-handler -> final compression order.
	incctx.Wrap(optAgent, dir)
	incctx.WrapGrep(optAgent, dir)
	wrapNormalize(optReg, dir, workspacePlaceholder)
	// Observe the same execution, rather than replaying tools to determine the
	// compression threshold (which would duplicate mutations). RunTask is serial.
	var logicalOut any
	for _, def := range optReg.List() {
		original := def.Handler
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			out, err := original(ctx, args)
			logicalOut = out
			return out, err
		}
	}
	toolcompress.WrapDynamic(optAgent, func(context.Context) int { return compressionThresholdTokens })

	counter := model.NewTokenCounter(evalModelName)
	res := TaskResult{TaskID: t.ID, Category: t.Category, Difficulty: t.Difficulty}

	for i, step := range t.Steps {
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

		baseTokens := jsonTokens(counter, baseOut, dir)
		res.BaselineTokens += baseTokens
		res.OptimizedTokens += jsonTokens(counter, optOut, dir)
		for _, violation := range ValidateStep(ctx, step, t.Files, logicalOut, optOut, optReg) {
			res.Violations = append(res.Violations, fmt.Sprintf("step %d (%s): %s", i+1, step.Tool, violation))
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
// Any occurrence of dir (the task's temp workspace, e.g. from
// os.MkdirTemp) is replaced with a fixed placeholder first: dir's absolute
// path is both random per run and environment-dependent (differs between a
// locally-generated baseline snapshot and CI's own temp dir), and several
// builtin tool results embed it verbatim (file_read/file_list/file_grep's
// "path" field) — left unnormalized, the suite's token counts would be
// nondeterministic and the CI gate would flag spurious regressions.
func jsonTokens(counter model.TokenCounter, v any, dir string) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	normalized := strings.ReplaceAll(string(data), dir, "/workspace")
	return counter.CountString(normalized)
}

// ValidateStep checks one step independently of prior reads. files is the
// immutable fixture oracle; logicalOut is the normalized pre-compression result
// from that execution. A compressed delivered result must be reconstructible via
// tools' public read_stored_result handler, not merely have a plausible preview.
// Retrieval is verifier IO, not an additional model trajectory step: token totals
// measure immediate tool responses, not the cost of a model reading every artifact.
func ValidateStep(ctx context.Context, step Step, files map[string]string, logicalOut, delivered any, tools *tool.Registry) []string {
	logicalJSON, err := json.Marshal(logicalOut)
	if err != nil {
		return []string{fmt.Sprintf("invalid logical result: %v", err)}
	}
	var logical map[string]any
	if err := json.Unmarshal(logicalJSON, &logical); err != nil || logical == nil {
		return []string{"logical result must be an object"}
	}
	var violations []string
	switch step.Tool {
	case "file_read":
		if err := validateRead(step, files, logical); err != nil {
			violations = append(violations, err.Error())
		}
	case "file_grep":
		if err := validateGrep(step, files, logical); err != nil {
			violations = append(violations, err.Error())
		}
	}
	out, ok := delivered.(map[string]any)
	if !ok || out == nil {
		return append(violations, "delivered result must be an object")
	}
	counter := model.NewTokenCounter(evalModelName)
	compressed, _ := out["compressed"].(bool)
	logicalTokens := counter.CountString(string(logicalJSON))
	if compressed != (logicalTokens > compressionThresholdTokens) {
		violations = append(violations, fmt.Sprintf("compressed=%t for %d logical tokens (threshold %d)", compressed, logicalTokens, compressionThresholdTokens))
	}
	if compressed && jsonTokens(counter, delivered, workspacePlaceholder) > compressionThresholdTokens {
		violations = append(violations, "compressed envelope exceeds token budget")
	}
	evidence := delivered
	if compressed {
		evidence, err = retrieveEvidence(ctx, out, tools)
		if err != nil {
			return append(violations, fmt.Sprintf("stored evidence: %v", err))
		}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil || !bytes.Equal(logicalJSON, evidenceJSON) {
		violations = append(violations, "delivered/retrieved evidence differs from the logical result")
	}
	return violations
}

func retrieveEvidence(ctx context.Context, out map[string]any, tools *tool.Registry) (any, error) {
	key, _ := out["storage_key"].(string)
	size, ok := exactInt(out["full_size_bytes"])
	// Offline verification must itself be bounded, even for a corrupt reader.
	if tools == nil || key == "" || !ok || size <= 0 || size > 4<<20 {
		return nil, fmt.Errorf("missing reader/key or invalid full_size_bytes")
	}
	var data strings.Builder
	for calls := 0; calls < 128; calls++ {
		chunk, err := tools.Execute(ctx, toolcompress.ReadStoredResultTool, map[string]any{"key": key, "offset": data.Len()})
		if err != nil {
			return nil, err
		}
		m, ok := chunk.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("chunk must be an object")
		}
		text, textOK := m["content"].(string)
		offset, offsetOK := exactInt(m["offset"])
		next, nextOK := exactInt(m["next_offset"])
		total, totalOK := exactInt(m["total_bytes"])
		more, moreOK := m["truncated"].(bool)
		if !textOK || !offsetOK || !nextOK || !totalOK || !moreOK ||
			m["storage_key"] != key || offset != data.Len() || next != offset+len(text) ||
			next <= offset || next > size || total != size || more != (next < size) {
			return nil, fmt.Errorf("invalid chunk coordinates/continuation")
		}
		data.WriteString(text)
		if !more {
			var evidence any
			if err := json.Unmarshal([]byte(data.String()), &evidence); err != nil {
				return nil, fmt.Errorf("decode stored result: %w", err)
			}
			return evidence, nil
		}
	}
	return nil, fmt.Errorf("stored result exceeded chunk limit")
}

func exactInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), float64(int(n)) == n
	default:
		return 0, false
	}
}

func validateRead(step Step, files map[string]string, out map[string]any) error {
	path, _ := step.Args["path"].(string)
	source, exists := files[path]
	if !exists {
		return fmt.Errorf("file_read: no fixture oracle for %q", path)
	}
	if out["path"] != filepath.Join(workspacePlaceholder, path) || out["unchanged"] == true || out["truncated"] == true {
		return fmt.Errorf("file_read: missing complete evidence/path for %s", path)
	}
	_, hasStart := step.Args["start_line"]
	_, hasEnd := step.Args["end_line"]
	outlineOnly, explicit := step.Args["outline_only"].(bool)
	expectedOutline, parseErr := declarationShape(source, false)
	shouldOutline := strings.HasSuffix(path, ".go") && !hasStart && !hasEnd &&
		(!explicit || outlineOnly) && (outlineOnly || len(source) > outlineThresholdBytes) &&
		len(source) <= 1<<20 && parseErr == nil && expectedOutline != "package evidence\n"
	if shouldOutline {
		decls, ok := out["declarations"].([]any)
		if out["outline"] != true || !ok || len(decls) == 0 {
			return fmt.Errorf("file_read: expected nonempty declaration outline for %s", path)
		}
		var text strings.Builder
		text.WriteString("package evidence\n")
		for _, decl := range decls {
			s, ok := decl.(string)
			if !ok {
				return fmt.Errorf("file_read: invalid declaration in %s", path)
			}
			text.WriteString(s + "\n")
		}
		actual, err := declarationShape(text.String(), true)
		if err != nil || actual != expectedOutline {
			return fmt.Errorf("file_read: incomplete/incorrect declaration outline for %s", path)
		}
		return nil
	}
	if out["outline"] == true {
		return fmt.Errorf("file_read: outline cannot satisfy requested source/range for %s", path)
	}
	lines := strings.Split(source, "\n")
	start, end := 1, len(lines)
	if hasStart {
		var ok bool
		start, ok = exactInt(step.Args["start_line"])
		if !ok || start < 1 || start > len(lines) {
			return fmt.Errorf("file_read: invalid requested start_line")
		}
	}
	if hasEnd {
		var ok bool
		end, ok = exactInt(step.Args["end_line"])
		if !ok || end < start {
			return fmt.Errorf("file_read: invalid requested end_line")
		}
		end = min(end, len(lines))
	}
	actualStart, startOK := exactInt(out["start_line"])
	actualEnd, endOK := exactInt(out["end_line"])
	if out["content"] != strings.Join(lines[start-1:end], "\n") || !startOK || !endOK || actualStart != start || actualEnd != end {
		return fmt.Errorf("file_read: incorrect source/range %s:%d-%d", path, start, end)
	}
	if total := out["total_lines"]; total != nil {
		if n, ok := exactInt(total); !ok || n != len(lines) {
			return fmt.Errorf("file_read: incorrect total_lines for %s", path)
		}
	}
	return nil
}

// Parse both sides independently and compare complete declaration structure,
// ignoring comments/package spelling. An outline cannot contain function bodies
// or omit/change signatures even if its outline=true marker looks convincing.
func declarationShape(source string, isOutline bool) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteString("package evidence\n")
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok {
			if isOutline && f.Body != nil {
				return "", fmt.Errorf("outline contains function body")
			}
			f.Body = nil
		}
		// Format each declaration separately: source positions must not make
		// empty lines left behind by removed bodies part of the comparison.
		if err := format.Node(&buf, fset, decl); err != nil {
			return "", err
		}
		buf.WriteByte('\n')
	}
	return buf.String(), nil
}

func validateGrep(step Step, files map[string]string, out map[string]any) error {
	path, _ := step.Args["path"].(string)
	pattern, _ := step.Args["pattern"].(string)
	matcher := func(line string) bool { return strings.Contains(line, pattern) }
	if step.Args["regex"] == true {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("file_grep: invalid fixture pattern: %w", err)
		}
		matcher = re.MatchString
	}
	_, single := files[path]
	if out["truncated"] == true || out["path"] != filepath.Join(workspacePlaceholder, path) || out["recursive"] != !single {
		return fmt.Errorf("file_grep: incomplete result or incorrect path/mode")
	}
	var expected []string
	for name, source := range files {
		if single && name != path || !single && path != "." && !strings.HasPrefix(name, strings.TrimSuffix(path, "/")+"/") {
			continue
		}
		for i, line := range strings.Split(source, "\n") {
			if matcher(line) {
				m := map[string]any{"line_number": i + 1, "content": line}
				if !single {
					m["file"] = filepath.Join(workspacePlaceholder, name)
				}
				encoded, _ := json.Marshal(m)
				expected = append(expected, string(encoded))
			}
		}
	}
	matches, ok := out["matches"].([]any)
	if !ok {
		return fmt.Errorf("file_grep: missing matches")
	}
	actual := make([]string, len(matches))
	for i, match := range matches {
		encoded, _ := json.Marshal(match)
		actual[i] = string(encoded)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		return fmt.Errorf("file_grep: incorrect matching evidence")
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
