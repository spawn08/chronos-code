package incctx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

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

	// Second read of the same, unchanged file must short-circuit as
	// "unchanged" without calling orig again.
	out2, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "big.go"})
	if err != nil {
		t.Fatalf("execute file_read (2nd): %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected orig handler still NOT to be called, got %d calls", counter.Load())
	}
	res2, ok := out2.(map[string]any)
	if !ok || res2["unchanged"] != true {
		t.Fatalf("expected unchanged result on 2nd read, got %#v", out2)
	}
}

func TestWrapMtimeChangeCallsOrigAgain(t *testing.T) {
	// Uses a small file (outlining never applies) so this test isolates the
	// P2-008 dedup-cache-invalidation behavior from P2-007 outlining: for a
	// large .go file the outline path would intercept every read and orig
	// would never be called, which would make it impossible to observe
	// "cache invalidated -> orig called again" in isolation.
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
	if counter.Load() != 1 {
		t.Fatalf("expected orig called once, got %d", counter.Load())
	}

	// Same mtime: dedup short-circuit, orig not called again.
	out, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"})
	if err != nil {
		t.Fatalf("execute file_read (2nd): %v", err)
	}
	if counter.Load() != 1 {
		t.Fatalf("expected orig still called once, got %d", counter.Load())
	}
	res, ok := out.(map[string]any)
	if !ok || res["unchanged"] != true {
		t.Fatalf("expected unchanged result, got %#v", out)
	}

	// Bump mtime into the future and rewrite content; cache must invalidate.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.WriteFile(path, []byte(smallGoFixture+"\n// changed\n"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes after rewrite: %v", err)
	}

	if _, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go"}); err != nil {
		t.Fatalf("execute file_read (3rd): %v", err)
	}
	if counter.Load() != 2 {
		t.Fatalf("expected orig called again after mtime change, got %d", counter.Load())
	}
}

func TestWrapForceAlwaysCallsOrigAndStripsForceKey(t *testing.T) {
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
	if counter.Load() != 1 {
		t.Fatalf("expected orig called once, got %d", counter.Load())
	}

	// Unchanged, but force=true must call orig anyway (the fake handler
	// itself panics if it sees a "force" key, verifying it never reaches
	// orig's args).
	if _, err := a.Tools.Execute(ctx, "file_read", map[string]any{"path": "small.go", "force": true}); err != nil {
		t.Fatalf("execute file_read (force): %v", err)
	}
	if counter.Load() != 2 {
		t.Fatalf("expected orig called again with force=true, got %d", counter.Load())
	}
}

func TestWrapPassesThroughSmallAndNonGoFiles(t *testing.T) {
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
	if counter.Load() != 1 {
		t.Fatalf("expected orig called once for small.go, got %d", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline for small.go, got %#v", out)
	}

	out, err = a.Tools.Execute(ctx, "file_read", map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatalf("execute file_read (big.txt): %v", err)
	}
	if counter.Load() != 2 {
		t.Fatalf("expected orig called for big.txt, got %d calls", counter.Load())
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
	if counter.Load() != 1 {
		t.Fatalf("expected orig called directly when start_line/end_line set, got %d", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline when start_line/end_line set, got %#v", out)
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
	if counter.Load() != 1 {
		t.Fatalf("expected orig called directly when outline_only=false, got %d", counter.Load())
	}
	if res, ok := out.(map[string]any); !ok || res["outline"] == true {
		t.Fatalf("expected no outline when outline_only=false, got %#v", out)
	}
}
