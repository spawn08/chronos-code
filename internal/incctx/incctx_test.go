package incctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestWrapOutlineThenDistinctRangesRegression(t *testing.T) {
	dir := t.TempDir()
	content := bigGoFixture()
	writeFile(t, dir, "big.go", content)
	a := newTestAgent(t)
	// Reproduce the original compression-before-slicing integration: orig
	// cannot supply source coordinates once its result has been compressed.
	a.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"content": "compressed summary"}, nil
		}})
	Wrap(a, dir)
	if _, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "big.go"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(content, "\n")
	for _, n := range []int{3, 15, 3} {
		out, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "big.go", "start_line": n, "end_line": n})
		if err != nil {
			t.Fatal(err)
		}
		if got := out.(map[string]any)["content"]; got != lines[n-1] {
			t.Fatalf("line %d = %v, want %q", n, got, lines[n-1])
		}
	}
}

func TestWrapInvalidRangesRegression(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lines.txt", "one\ntwo\nthree")
	for _, args := range []map[string]any{
		{"start_line": 99}, {"start_line": 0}, {"start_line": -1},
		{"start_line": 3, "end_line": 2}, {"end_line": 0},
		{"start_line": 1.5}, {"start_line": "2"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			a := newTestAgent(t)
			var calls atomic.Int64
			registerFakeFileRead(a, dir, &calls)
			Wrap(a, dir)
			args["path"] = "lines.txt"
			_, err := a.Tools.Execute(context.Background(), "file_read", args)
			if err == nil || strings.Contains(err.Error(), "panicked") {
				t.Fatalf("want actionable range error, got %v", err)
			}
		})
	}
}

func TestWrapCanceledRegression(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lines.txt", "target")
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileRead(a, dir, &calls)
	registerFakeFileGrep(a, dir, &calls)
	Wrap(a, dir)
	WrapGrep(a, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{"file_read", "file_grep"} {
		_, err := a.Tools.Execute(ctx, name, map[string]any{"path": "lines.txt", "pattern": "target"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: want cancellation, got %v", name, err)
		}
	}
}

func TestWrapUsesRequestWorkspaceRoot(t *testing.T) {
	configured := t.TempDir()
	request := t.TempDir()
	writeFile(t, configured, "same.txt", "configured marker")
	writeFile(t, request, "same.txt", "request marker")
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileRead(a, configured, &calls)
	registerFakeFileGrep(a, configured, &calls)
	Wrap(a, configured)
	WrapGrep(a, configured)
	ctx := builtins.WithWorkspaceRoot(context.Background(), request)

	read, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "same.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := read.(map[string]any)["content"]; got != "request marker" {
		t.Fatalf("file_read content = %q, want request workspace", got)
	}
	grep, err := a.Tools.Execute(ctx, "file_grep", map[string]any{"path": ".", "pattern": "marker"})
	if err != nil {
		t.Fatal(err)
	}
	matches := grep.(map[string]any)["matches"].([]map[string]any)
	if len(matches) != 1 || matches[0]["content"] != "request marker" {
		t.Fatalf("file_grep matches = %#v, want request workspace only", matches)
	}
}

func TestWrapRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := writeFile(t, outside, "outside.txt", "secret")
	if err := os.Symlink(target, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileRead(a, root, &calls)
	Wrap(a, root)
	if _, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "escape.txt"}); err == nil || !strings.Contains(err.Error(), "outside request workspace") {
		t.Fatalf("file_read symlink escape error = %v", err)
	}
}

type fakeProvider struct{}

func (fakeProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}
func (fakeProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}
func (fakeProvider) Name() string  { return "fake" }
func (fakeProvider) Model() string { return "claude-sonnet-4-6" }

func newTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	return &agent.Agent{
		ID:    "test-agent",
		Model: fakeProvider{},
		Tools: tool.NewRegistry(),
	}
}

// bodySecret is a unique marker placed inside a function body in the
// synthetic fixture; it must never appear in an outline (outlines carry
// signatures only, never bodies).
const bodySecret = "UNIQUE_BODY_MARKER_7f3a9c"

// bigGoFixture returns Go source with several real top-level declarations
// that is padded well past the 2000-byte outline threshold.
func bigGoFixture() string {
	var b strings.Builder
	b.WriteString("package fixture\n\n")
	b.WriteString("const DefaultLimit = 100\n\n")
	b.WriteString("var GlobalCounter int\n\n")
	b.WriteString("// Store holds some data.\ntype Store struct {\n\tName string\n\tdata map[string]string\n}\n\n")
	b.WriteString("// FindSymbols looks up symbols by name and kind.\n")
	b.WriteString("func (s *Store) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {\n")
	b.WriteString("\tsecret := \"" + bodySecret + "\"\n\t_ = secret\n\treturn nil, nil\n}\n\n")
	b.WriteString("// Helper pads the file out with a large comment block so the fixture\n")
	b.WriteString("// comfortably exceeds the outline size threshold used by incctx.\n")
	b.WriteString("func Helper(x int, y int) int {\n")
	// padding to push size well over 2000 bytes
	for i := 0; i < 40; i++ {
		b.WriteString("\t// padding line to inflate file size for outline threshold testing\n")
	}
	b.WriteString("\treturn x + y\n}\n\n")
	b.WriteString("func AnotherFunc(s string) string {\n\treturn s\n}\n")
	return b.String()
}

const smallGoFixture = `package fixture

func Tiny() int {
	return 1
}
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// registerFakeFileRead registers a stub file_read tool whose Handler
// increments counter each time it's invoked and returns the file's content,
// so tests can assert whether the wrapped handler delegated to it or not.
// Like the real file_read tool, relative paths are resolved against root.
func registerFakeFileRead(a *agent.Agent, root string, counter *atomic.Int64) {
	a.Tools.Register(&tool.Definition{
		Name:       "file_read",
		Permission: tool.PermAllow,
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			counter.Add(1)
			if _, has := args["force"]; has {
				panic("force key leaked into orig handler args")
			}
			path, _ := args["path"].(string)
			resolved := path
			if !filepath.IsAbs(path) {
				resolved = filepath.Join(root, path)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return nil, err
			}
			return map[string]any{"content": string(data)}, nil
		},
	})
}

func TestWrapNoFileReadTool(t *testing.T) {
	a := newTestAgent(t)
	// No file_read tool registered; Wrap must be a no-op and not panic.
	Wrap(a, t.TempDir())
	if _, ok := a.Tools.Get("file_read"); ok {
		t.Fatal("expected no file_read tool to exist")
	}
}

func TestWrapOutlinesLargeGoFile(t *testing.T) {
	dir := t.TempDir()
	content := bigGoFixture()
	if len(content) <= outlineSizeThreshold {
		t.Fatalf("fixture too small for test: %d bytes", len(content))
	}
	writeFile(t, dir, "big.go", content)

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "big.go"})
	if err != nil {
		t.Fatalf("execute file_read: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected orig handler NOT to be called, got %d calls", counter.Load())
	}
	res, ok := out.(map[string]any)
	if !ok || res["outline"] != true {
		t.Fatalf("expected an outline result, got %#v", out)
	}
	decls, ok := res["declarations"].([]string)
	if !ok || len(decls) == 0 {
		t.Fatalf("expected non-empty declarations, got %#v", res["declarations"])
	}
	joined := strings.Join(decls, "\n")
	if !strings.Contains(joined, "FindSymbols") {
		t.Errorf("expected outline to mention FindSymbols, got:\n%s", joined)
	}
	if !strings.Contains(joined, "Helper") {
		t.Errorf("expected outline to mention Helper, got:\n%s", joined)
	}
	if strings.Contains(joined, bodySecret) {
		t.Errorf("outline leaked function body content: %s", joined)
	}

	// Repeated reads must still supply the representation: earlier context
	// may have been compacted, and mtime is not proof of model coverage.
	out2, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "big.go"})
	if err != nil {
		t.Fatalf("execute file_read (2nd): %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected orig handler still NOT to be called, got %d calls", counter.Load())
	}
	res2, ok := out2.(map[string]any)
	if !ok || res2["outline"] != true || res2["unchanged"] == true {
		t.Fatalf("expected fresh outline on 2nd read, got %#v", out2)
	}
}

func TestWrapFreshContentEvenWithSameMtime(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "small.go", smallGoFixture)

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()

	if _, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"}); err != nil {
		t.Fatalf("execute file_read (1st): %v", err)
	}
	// Same mtime: content must still be returned.
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"})
	if err != nil {
		t.Fatalf("execute file_read (2nd): %v", err)
	}
	res, ok := out.(map[string]any)
	if !ok || res["content"] != smallGoFixture {
		t.Fatalf("expected content, got %#v", out)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(smallGoFixture+"\n// changed\n"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	if err := os.Chtimes(path, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatalf("chtimes after rewrite: %v", err)
	}

	out, err = a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"})
	if err != nil {
		t.Fatalf("execute file_read (3rd): %v", err)
	}
	if out.(map[string]any)["content"] != smallGoFixture+"\n// changed\n" {
		t.Fatalf("stale content: %#v", out)
	}
}

func TestWrapForceReturnsContentWithoutMutatingArgs(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "small.go", smallGoFixture)
	_ = path

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()

	if _, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"}); err != nil {
		t.Fatalf("execute file_read (1st): %v", err)
	}
	args := map[string]any{"path": "small.go", "force": true}
	out, err := a.Tools.Execute(ctx, "file_read", args)
	if err != nil {
		t.Fatalf("execute file_read (force): %v", err)
	}
	if args["force"] != true || out.(map[string]any)["content"] != smallGoFixture {
		t.Fatalf("args = %#v, result = %#v", args, out)
	}
}

func TestWrapReadsSmallAndNonGoFiles(t *testing.T) {
	dir := t.TempDir()
	smallGoPath := writeFile(t, dir, "small.go", smallGoFixture)
	_ = smallGoPath

	// A non-.go file padded past the outline threshold, to prove suffix
	// matters, not just size.
	bigTextContent := strings.Repeat("not go source, just padding text\n", 100)
	writeFile(t, dir, "big.txt", bigTextContent)

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()

	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"})
	if err != nil {
		t.Fatalf("execute file_read (small.go): %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected bounded direct IO, got %d orig calls", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline for small.go, got %#v", out)
	}

	out, err = a.Tools.Execute(ctx, "file_read", map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatalf("execute file_read (big.txt): %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected bounded direct IO, got %d orig calls", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline for big.txt, got %#v", out)
	}
}

func TestWrapStartLineEndLineSkipsOutline(t *testing.T) {
	dir := t.TempDir()
	content := bigGoFixture()
	writeFile(t, dir, "big.go", content)

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{
		"path":       "big.go",
		"start_line": 1,
		"end_line":   5,
	})
	if err != nil {
		t.Fatalf("execute file_read: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("range must not call the whole-file handler, got %d calls", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline when start_line/end_line set, got %#v", out)
	}
}

func TestWrapSlicesContentToRequestedLineRange(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lines.txt", "one\ntwo\nthree\nfour\nfive")

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{
		"path":       "lines.txt",
		"start_line": 2,
		"end_line":   3,
	})
	if err != nil {
		t.Fatalf("execute file_read: %v", err)
	}
	res, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %#v", out)
	}
	if res["content"] != "two\nthree" {
		t.Errorf("content = %q, want %q", res["content"], "two\nthree")
	}
	if res["total_lines"] != nil {
		t.Errorf("total_lines = %v, want unknown for early range", res["total_lines"])
	}
}

func TestWrapClampsOutOfRangeLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lines.txt", "one\ntwo\nthree")

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{
		"path":       "lines.txt",
		"start_line": 2,
		"end_line":   999,
	})
	if err != nil {
		t.Fatalf("execute file_read: %v", err)
	}
	res := out.(map[string]any)
	if res["content"] != "two\nthree" {
		t.Errorf("content = %q, want %q", res["content"], "two\nthree")
	}
}

func TestWrapDeclaresLineRangeParameters(t *testing.T) {
	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, t.TempDir(), &counter)
	Wrap(a, t.TempDir())

	def, ok := a.Tools.Get("file_read")
	if !ok {
		t.Fatal("expected file_read tool to exist")
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %#v", def.Parameters)
	}
	if _, ok := props["start_line"]; !ok {
		t.Error("expected start_line to be declared in Parameters")
	}
	if _, ok := props["end_line"]; !ok {
		t.Error("expected end_line to be declared in Parameters")
	}
	for _, key := range []string{"force", "outline_only"} {
		if props[key].(map[string]any)["type"] != "boolean" {
			t.Errorf("expected boolean schema for %s", key)
		}
	}
}

// registerFakeFileGrep registers a stub file_grep tool matching the SDK
// builtin's single-file, plain-substring contract, so tests can assert
// whether WrapGrep delegated to it or handled the call itself.
func registerFakeFileGrep(a *agent.Agent, root string, counter *atomic.Int64) {
	a.Tools.Register(&tool.Definition{
		Name:       "file_grep",
		Permission: tool.PermAllow,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			counter.Add(1)
			p, _ := args["path"].(string)
			pattern, _ := args["pattern"].(string)
			resolved := p
			if !filepath.IsAbs(p) {
				resolved = filepath.Join(root, p)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return nil, err
			}
			var matches []map[string]any
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, pattern) {
					matches = append(matches, map[string]any{"line_number": i + 1, "content": line})
				}
			}
			return map[string]any{"path": resolved, "pattern": pattern, "matches": matches}, nil
		},
	})
}

func TestWrapGrepNoFileGrepTool(t *testing.T) {
	a := newTestAgent(t)
	WrapGrep(a, t.TempDir())
	if _, ok := a.Tools.Get("file_grep"); ok {
		t.Fatal("expected no file_grep tool to exist")
	}
}

func TestWrapGrepSingleFileUsesBoundedIO(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test.txt", "foo\nbar\nbaz")

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileGrep(a, dir, &counter)
	WrapGrep(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_grep", map[string]any{"path": "test.txt", "pattern": "bar"})
	if err != nil {
		t.Fatalf("execute file_grep: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected bounded direct IO, got %d orig calls", counter.Load())
	}
	res := out.(map[string]any)
	matches := res["matches"].([]map[string]any)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

func TestWrapGrepRecursiveSkipsVendor(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, "vendor"), 0o755)
	writeFile(t, dir, "a.txt", "target here")
	writeFile(t, filepath.Join(dir, "sub"), "b.txt", "target there")
	writeFile(t, filepath.Join(dir, "vendor"), "c.txt", "target ignored")

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileGrep(a, dir, &counter)
	WrapGrep(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_grep", map[string]any{"path": ".", "pattern": "target"})
	if err != nil {
		t.Fatalf("execute file_grep: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected recursive search NOT to delegate to orig, got %d calls", counter.Load())
	}
	res := out.(map[string]any)
	if res["recursive"] != true {
		t.Error("expected recursive = true")
	}
	matches := res["matches"].([]map[string]any)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (vendor skipped), got %d", len(matches))
	}
}

func TestWrapGrepRegexAlternation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test.txt", "foo\nbar\nbaz")

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileGrep(a, dir, &counter)
	WrapGrep(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_grep", map[string]any{
		"path":    "test.txt",
		"pattern": "foo|baz",
		"regex":   true,
	})
	if err != nil {
		t.Fatalf("execute file_grep: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected regex search NOT to delegate to the substring-only orig, got %d calls", counter.Load())
	}
	res := out.(map[string]any)
	matches := res["matches"].([]map[string]any)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
}

func TestWrapGrepDeclaresRegexParameter(t *testing.T) {
	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileGrep(a, t.TempDir(), &counter)
	WrapGrep(a, t.TempDir())

	def, ok := a.Tools.Get("file_grep")
	if !ok {
		t.Fatal("expected file_grep tool to exist")
	}
	props := def.Parameters["properties"].(map[string]any)
	if _, ok := props["regex"]; !ok {
		t.Error("expected regex to be declared in Parameters")
	}
}

func TestWrapOutlineOnlyFalseForcesFullContent(t *testing.T) {
	dir := t.TempDir()
	content := bigGoFixture()
	writeFile(t, dir, "big.go", content)

	a := newTestAgent(t)
	var counter atomic.Int64
	registerFakeFileRead(a, dir, &counter)
	Wrap(a, dir)

	ctx := context.Background()
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{
		"path":         "big.go",
		"outline_only": false,
	})
	if err != nil {
		t.Fatalf("execute file_read: %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected bounded direct IO, got %d orig calls", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline when outline_only=false, got %#v", out)
	}
}

func TestWrapRangeIgnoresCompressedOriginal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lines.txt", "one\ntwo\nthree")
	a := newTestAgent(t)
	a.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"content": "summary"}, nil
		}})
	Wrap(a, dir)
	out, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "lines.txt", "start_line": 2, "end_line": 2})
	if err != nil || out.(map[string]any)["content"] != "two" {
		t.Fatalf("want raw second line, got %#v, %v", out, err)
	}
}

func TestWrapLineBoundarySemantics(t *testing.T) {
	for _, content := range []string{"", "one", "one\n", "one\r\ntwo\r\n", "\n\n"} {
		t.Run(fmt.Sprintf("%q", content), func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "lines.txt", content)
			a := newTestAgent(t)
			var calls atomic.Int64
			registerFakeFileRead(a, dir, &calls)
			Wrap(a, dir)
			out, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "lines.txt"})
			if err != nil {
				t.Fatal(err)
			}
			res := out.(map[string]any)
			if res["content"] != content || res["total_lines"] != len(strings.Split(content, "\n")) || res["path"] != filepath.Join(dir, "lines.txt") {
				t.Fatalf("bad content/metadata: %#v", res)
			}
		})
	}
}

func TestWrapHugeFileBoundsAndContinuation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.go")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	// A sparse 1 GiB file makes whole-file reads observable without a 1 GiB
	// fixture allocation. The bounded text prefix alone exceeds output limits.
	if _, err := io.WriteString(f, strings.Repeat(strings.Repeat("x", 1000)+"\n", 300)); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)
	a.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) {
			t.Fatal("must not call whole-file handler")
			return nil, nil
		}})
	Wrap(a, dir)
	for _, args := range []map[string]any{{"path": "huge.go"}, {"path": "huge.go", "force": true, "outline_only": false}} {
		out, err := a.Tools.Execute(context.Background(), "file_read", args)
		if err != nil {
			t.Fatal(err)
		}
		res := out.(map[string]any)
		if len(res["content"].(string)) > maxOutputBytes || res["truncated"] != true || res["total_lines"] != nil {
			t.Fatalf("incorrect bounded metadata: %#v", res)
		}
		next := res["next_start_line"].(int)
		out, err = a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "huge.go", "start_line": next, "end_line": next})
		if err != nil || out.(map[string]any)["content"] != strings.Repeat("x", 1000) {
			t.Fatalf("continuation failed: %#v, %v", out, err)
		}
	}
}

func TestWrapRejectsOverlongLinesAndInvalidNumbers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "long.txt", strings.Repeat("x", maxLineBytes+1))
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileRead(a, dir, &calls)
	Wrap(a, dir)
	_, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "long.txt"})
	if !errors.Is(err, errLongLine) {
		t.Fatalf("want bounded line error, got %v", err)
	}
	for _, n := range []float64{math.Inf(1), math.NaN(), float64(math.MaxInt)} {
		_, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "long.txt", "start_line": n})
		if err == nil || !strings.Contains(err.Error(), "positive integer") {
			t.Fatalf("invalid line %v: %v", n, err)
		}
	}
}

type countingReader struct {
	r     io.Reader
	bytes int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += n
	return n, err
}

type cancelingReader struct {
	cancel context.CancelFunc
}

func (r cancelingReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "last line"), io.EOF
}

func TestScanStopsAtRangeByteBudgetAndCancellation(t *testing.T) {
	// More than one buffer of lines: test both buffered-line cancellation
	// and stopping before reading the remainder of a file.
	data := strings.Repeat("line\n", maxLineBytes)
	t.Run("range", func(t *testing.T) {
		r := &countingReader{r: strings.NewReader(data)}
		_, err := scanLines(context.Background(), r, maxScanBytes, func(n int, line []byte) error { return errStopScan })
		if err != nil || r.bytes > maxLineBytes+1 {
			t.Fatalf("read past bounded range: bytes=%d, err=%v", r.bytes, err)
		}
	})
	t.Run("budget", func(t *testing.T) {
		r := &countingReader{r: strings.NewReader(data)}
		stats, err := scanLines(context.Background(), r, 100, func(n int, line []byte) error { return nil })
		if !errors.Is(err, errScanLimit) || r.bytes != 100 || stats.bytes != 100 || stats.complete {
			t.Fatalf("budget not enforced: %+v, bytes=%d, err=%v", stats, r.bytes, err)
		}
	})
	t.Run("cancel during scan", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		visited := 0
		_, err := scanLines(ctx, strings.NewReader(data), maxScanBytes, func(n int, line []byte) error {
			visited++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || visited != 1 {
			t.Fatalf("cancel not observed inside scan: visited=%d, err=%v", visited, err)
		}
	})
	t.Run("cancel during final IO", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := scanLines(ctx, cancelingReader{cancel: cancel}, maxScanBytes, func(n int, line []byte) error {
			t.Fatal("must not publish content after canceled IO")
			return errStopScan
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("last read lost cancellation: %v", err)
		}
	})
}

func TestWrapGrepSkipsRuntimeBinaryAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	for name := range grepSkipDirs {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), "ignored.txt", "target")
	}
	writeFile(t, dir, "good.txt", "target")
	writeFile(t, dir, "binary.dat", "target\n\x00target")
	writeFile(t, dir, "invalid.dat", "target\xff")
	if err := os.Symlink(filepath.Join(dir, "good.txt"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileGrep(a, dir, &calls)
	WrapGrep(a, dir)
	out, err := a.Tools.Execute(context.Background(), "file_grep", map[string]any{"path": ".", "pattern": "target"})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	matches := res["matches"].([]map[string]any)
	if len(matches) != 1 || filepath.Base(matches[0]["file"].(string)) != "good.txt" || res["truncated"] != false {
		t.Fatalf("unwanted search results: %#v", res)
	}
}

func TestWrapGrepOutputBounds(t *testing.T) {
	for _, content := range []string{strings.Repeat("target\n", 1000), strings.Repeat(strings.Repeat("target", 1000)+"\n", 100), strings.Repeat("x", maxLineBytes+1)} {
		dir := t.TempDir()
		writeFile(t, dir, "matches.txt", content)
		a := newTestAgent(t)
		var calls atomic.Int64
		registerFakeFileGrep(a, dir, &calls)
		WrapGrep(a, dir)
		for _, path := range []string{"matches.txt", "."} {
			out, err := a.Tools.Execute(context.Background(), "file_grep", map[string]any{"path": path, "pattern": "target"})
			if err != nil {
				t.Fatal(err)
			}
			res := out.(map[string]any)
			encoded, err := json.Marshal(res["matches"])
			if err != nil {
				t.Fatal(err)
			}
			if res["truncated"] != true || len(res["matches"].([]map[string]any)) > grepMaxMatches || len(encoded) > maxOutputBytes {
				t.Fatalf("bounds missing: truncated=%v, bytes=%d", res["truncated"], len(encoded))
			}
		}
	}
}

func TestGrepCancellationDuringRecursiveScan(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "first\nsecond\nthird")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visited := 0
	s := grepSearch{remaining: grepMaxScanBytes, matcher: func([]byte) bool {
		visited++
		cancel()
		return false
	}}
	if err := s.walk(ctx, dir, 0); !errors.Is(err, context.Canceled) || visited != 1 {
		t.Fatalf("cancellation swallowed: visited=%d, err=%v", visited, err)
	}
}

func TestGrepAggregateAndTraversalBudgets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", strings.Repeat("no match\n", 100))
	for _, s := range []*grepSearch{
		{remaining: 100, matcher: func([]byte) bool { return false }},
		{remaining: grepMaxScanBytes, entries: grepMaxEntries, matcher: func([]byte) bool { t.Fatal("entry budget exceeded"); return false }},
	} {
		if err := s.walk(context.Background(), dir, 0); err != nil || !s.truncated {
			t.Fatalf("budget ignored: %+v, %v", s, err)
		}
	}
	s := grepSearch{remaining: grepMaxScanBytes}
	if err := s.walk(context.Background(), dir, grepMaxDepth); err != nil || !s.truncated {
		t.Fatalf("depth ignored: %+v, %v", s, err)
	}
}

func TestWrapOuterPermissionsAndHooks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "target")
	a := newTestAgent(t)
	var calls atomic.Int64
	registerFakeFileRead(a, dir, &calls)
	registerFakeFileGrep(a, dir, &calls)
	Wrap(a, dir)
	WrapGrep(a, dir)
	for _, name := range []string{"file_read", "file_grep"} {
		def, _ := a.Tools.Get(name)
		original := def.Handler
		pre, post := 0, 0
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			pre++
			out, err := original(ctx, args)
			post++
			return out, err
		}
		args := map[string]any{"path": "a.txt", "pattern": "target"}
		def.Permission = tool.PermDeny
		if _, err := a.Tools.Execute(context.Background(), name, args); err == nil || pre != 0 {
			t.Fatalf("%s: registry denial bypassed", name)
		}
		def.Permission = tool.PermRequireApproval
		if _, err := a.Tools.Execute(context.Background(), name, args); err == nil || pre != 0 {
			t.Fatalf("%s: approval bypassed", name)
		}
		def.Permission = tool.PermAllow
		if _, err := a.Tools.Execute(context.Background(), name, args); err != nil || pre != 1 || post != 1 {
			t.Fatalf("%s: outer hooks did not execute once: pre=%d post=%d err=%v", name, pre, post, err)
		}
	}
}
