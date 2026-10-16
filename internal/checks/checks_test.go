package checks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos-code/internal/execution"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "chronos-checks-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for path, data := range files {
		put(t, root, path, data)
	}
	return root
}
func put(t *testing.T, root, path, data string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func directory(path string) execution.Scope {
	if path == "." {
		return execution.Scope{Kind: execution.ScopeWorkspace}
	}
	return execution.Scope{Kind: execution.ScopeDirectory, Path: path}
}
func exact(path string) execution.Scope {
	return execution.Scope{Kind: execution.ScopeExact, Path: path}
}

func TestSelectNearestManifestsAndAffectedPaths(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module example.test/root\n", "main.go": "package root\n",
		"nested module/go.mod": "module example.test/nested\n", "nested module/pkg/x.go": "package pkg\n",
		"python/pyproject.toml": "[tool.pytest.ini_options]\n", "python/lib/a.py": "",
		"python/service/pytest.ini": "[pytest]\n", "python/service/app.py": "",
		"rust/Cargo.toml": "[package]\nname = 'root'\n", "rust/src/lib.rs": "",
		"web/package.json": `{"scripts":{"test":"jest"},"packageManager":"pnpm@9.0.0"}`,
		"web/src/a.ts":     "", "web/tsconfig.json": "{}",
		"web/nested/package.json": `{"scripts":{"typecheck":"tsc --noEmit"}}`, "web/nested/src/a.ts": "",
	})
	changed := []string{"main.go", "nested module/pkg/x.go", "nested module/pkg/x.go", "python/lib/a.py", "python/service/app.py", "rust/src/lib.rs", "web/src/a.ts", "web/tsconfig.json", "web/nested/src/a.ts"}
	got, err := Select(context.Background(), root, changed)
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{
		{Family: "go-test", Root: ".", Command: "go test ./...", Class: execution.CommandTest, Paths: []string{"main.go"}},
		{Family: "go-test", Root: "nested module", Command: "cd 'nested module' && go test ./...", Class: execution.CommandTest, Paths: []string{"nested module/pkg/x.go"}},
		{Family: "javascript-diagnostics", Root: "web/nested", Command: "cd web/nested && pnpm run typecheck", Class: execution.CommandDiagnostics, Paths: []string{"web/nested/src/a.ts"}},
		{Family: "javascript-test", Root: "web", Command: "cd web && pnpm run test", Class: execution.CommandTest, Paths: []string{"web/src/a.ts", "web/tsconfig.json"}},
		{Family: "python-test", Root: "python", Command: "cd python && python -m pytest", Class: execution.CommandTest, Paths: []string{"python/lib/a.py"}},
		{Family: "python-test", Root: "python/service", Command: "cd python/service && python -m pytest", Class: execution.CommandTest, Paths: []string{"python/service/app.py"}},
		{Family: "rust-test", Root: "rust", Command: "cd rust && cargo test", Class: execution.CommandTest, Paths: []string{"rust/src/lib.rs"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Select = %#v, want %#v", got, want)
	}
}

func TestSelectPythonManifests(t *testing.T) {
	for _, manifest := range []string{"pyproject.toml", "pytest.ini", "setup.cfg", "setup.py", "requirements.txt"} {
		t.Run(manifest, func(t *testing.T) {
			root := fixture(t, map[string]string{manifest: "", "app.py": ""})
			for _, changed := range [][]string{{"app.py"}, {manifest}} {
				got, err := Select(context.Background(), root, changed)
				if err != nil || len(got) != 1 || got[0].Family != "python-test" || !reflect.DeepEqual(got[0].Paths, changed) {
					t.Fatalf("Select(%v) = %#v, %v", changed, got, err)
				}
			}
		})
	}
}

func TestSelectScriptsAndManagers(t *testing.T) {
	cases := []struct {
		name, pkg, lock, command, family string
		class                            execution.CommandClass
	}{
		{"npm", `{"scripts":{"test":"jest","build":"vite build"}}`, "package-lock.json", "npm run test", "javascript-test", execution.CommandTest},
		{"yarn", `{"scripts":{"test":"jest"}}`, "yarn.lock", "yarn run test", "javascript-test", execution.CommandTest},
		{"pnpm", `{"scripts":{"check":"tsc --noEmit"}}`, "pnpm-lock.yaml", "pnpm run check", "javascript-diagnostics", execution.CommandDiagnostics},
		{"bun", `{"scripts":{"lint":"eslint ."}}`, "bun.lockb", "bun run lint", "javascript-diagnostics", execution.CommandDiagnostics},
		{"build", `{"scripts":{"build":"vite build"}}`, "", "npm run build", "javascript-build", execution.CommandBuild},
		{"declared", `{"packageManager":"yarn@4.0.0","scripts":{"typecheck":"tsc --noEmit","check":"tsc"}}`, "pnpm-lock.yaml", "yarn run typecheck", "javascript-diagnostics", execution.CommandDiagnostics},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"package.json": tc.pkg, "src/main.ts": ""}
			if tc.lock != "" {
				files[tc.lock] = ""
			}
			root := fixture(t, files)
			changed := []string{"package.json", "src/main.ts"}
			if tc.lock != "" {
				changed = append(changed, tc.lock)
			}
			got, err := Select(context.Background(), root, changed)
			if err != nil || len(got) != 1 {
				t.Fatalf("Select = %#v, %v", got, err)
			}
			if got[0].Command != tc.command || got[0].Family != tc.family || got[0].Class != tc.class || len(got[0].Paths) != len(changed) {
				t.Fatalf("Select = %#v", got)
			}
		})
	}
	root := fixture(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`, "a.ts": ""})
	got, err := Select(context.Background(), root, []string{"a.ts"})
	if err != nil || len(got) != 0 {
		t.Fatalf("invented script: %#v, %v", got, err)
	}
	root = fixture(t, map[string]string{"a.go": "", "a.py": "", "a.rs": "", "a.ts": ""})
	got, err = Select(context.Background(), root, []string{"a.go", "a.py", "a.rs", "a.ts"})
	if err != nil || len(got) != 0 {
		t.Fatalf("invented manifests: %#v, %v", got, err)
	}
}

func TestInferScopes(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module root\n", "main.go": "package root\n",
		"pkg/a.go": "package pkg\n", "pkg/a_test.go": "package pkg\n", "pkg/deep/b.go": "package deep\n",
		"nested module/go.mod": "module nested\n", "nested module/a.go": "package nested\n",
		"python/pyproject.toml": "", "python/tests/test one.py": "", "python/tests/test_two.py": "",
		"python/child/pytest.ini": "", "python/child/tests/test_child.py": "",
		"rust/Cargo.toml":     "[workspace]\nmembers = [\"one\", \"two\"]\n",
		"rust/one/Cargo.toml": "[package]\nname = \"one\"\n", "rust/one/src/lib.rs": "",
		"rust/two/Cargo.toml": "[package]\nname = \"two\"\n", "rust/two/src/lib.rs": "",
		"web/package.json":      `{"scripts":{"test":"vitest run","typecheck":"tsc --noEmit","build":"vite build","lint":"eslint src"}}`,
		"web/tests/test one.ts": "", "web/src/a.ts": "", "web/nested/package.json": `{"scripts":{"test":"jest"}}`, "web/nested/test.ts": "",
	})
	cases := []struct {
		name, cwd, command, family, evidenceRoot string
		scopes                                   []execution.Scope
	}{
		{"go recursive", ".", "go test -race -count=1 ./...", "go-test", ".", []execution.Scope{directory(".")}},
		{"go directory recursive", ".", "go test ./pkg/...", "go-test", ".", []execution.Scope{directory("pkg")}},
		{"go package nonrecursive", ".", "go test ./pkg", "go-test", ".", []execution.Scope{exact("pkg/a.go"), exact("pkg/a_test.go")}},
		{"go default package", ".", "go test", "go-test", ".", []execution.Scope{exact("main.go")}},
		{"go explicit file", ".", "go test pkg/a_test.go", "go-test", ".", []execution.Scope{exact("pkg/a_test.go")}},
		{"go nested", "nested module", "go test ./...", "go-test", "nested module", []execution.Scope{directory("nested module")}},
		{"go env time", ".", "time -p env CI=1 go test ./pkg/...", "go-test", ".", []execution.Scope{directory("pkg")}},
		{"go build", ".", "go build ./pkg/...", "go-build", ".", []execution.Scope{directory("pkg")}},
		{"pytest full", "python", "python3 -m pytest -q", "python-test", "python", []execution.Scope{directory("python")}},
		{"pytest subdir", "python/tests", "pytest .", "python-test", "python", []execution.Scope{directory("python/tests")}},
		{"pytest nodes quoted", "python", `pytest 'tests/test one.py::TestA::test_a'`, "python-test", "python", []execution.Scope{exact("python/tests/test one.py")}},
		{"pytest directory", "python", `pytest tests`, "python-test", "python", []execution.Scope{directory("python/tests")}},
		{"pytest nested", "python/child", "pytest", "python-test", "python/child", []execution.Scope{directory("python/child")}},
		{"cargo manifest", ".", `cargo test --manifest-path rust/one/Cargo.toml`, "rust-test", "rust/one", []execution.Scope{directory("rust/one")}},
		{"cargo workspace", "rust", "cargo test --workspace", "rust-test", "rust", []execution.Scope{directory("rust")}},
		{"cargo package", "rust", "cargo test -p two", "rust-test", "rust/two", []execution.Scope{directory("rust/two")}},
		{"cargo build", "rust/one/src", "cargo build --release", "rust-build", "rust/one", []execution.Scope{directory("rust/one")}},
		{"js npm", "web", "npm test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js yarn", "web", "yarn test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js pnpm", "web", "pnpm run test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js bun script", "web", "bun run test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js bun native", "web", "bun test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js quoted test path", "web", `npm run test -- 'tests/test one.ts'`, "javascript-test", "web", []execution.Scope{exact("web/tests/test one.ts")}},
		{"js directory", "web", "pnpm test tests", "javascript-test", "web", []execution.Scope{directory("web/tests")}},
		{"js nested", "web/nested", "yarn test", "javascript-test", "web/nested", []execution.Scope{directory("web/nested")}},
		{"js invoked subdir", "web/src", "npm test", "javascript-test", "web", []execution.Scope{directory("web")}},
		{"js diagnostics", "web", "npm run typecheck", "javascript-diagnostics", "web", []execution.Scope{directory("web")}},
		{"js lint scoped", "web", "yarn lint", "javascript-diagnostics", "web", []execution.Scope{directory("web/src")}},
		{"js build", "web", "bun run build", "javascript-build", "web", []execution.Scope{directory("web")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(context.Background(), root, tc.cwd, tc.command)
			want := CommandEvidence{Family: tc.family, Root: tc.evidenceRoot, Scopes: tc.scopes, Known: true}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("Infer(%q) = %#v, %v, want %#v", tc.command, got, err, want)
			}
		})
	}
}

func TestInferUnknownSelectorsNeverHaveCoverage(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module root\n", "a.go": "package root\n", "pyproject.toml": "", "tests/test_a.py": "",
		"Cargo.toml": "[package]\nname='one'\n", "src/lib.rs": "",
		"package.json": `{"scripts":{"test":"vitest run","typecheck":"tsc --noEmit","build":"vite build"}}`, "tests/a.test.ts": "",
	})
	commands := []string{
		"go test all", "go test example.org/pkg", "go test ./missing/...", "go test ./... -run TestOne", "go test ./... -tags integration",
		"go test ./... -args -test.run X", "go test ./... ./missing", "go test ./... -count", "GOFLAGS='-run TestOne' go test ./...",
		"pytest -k one", "pytest -m fast", "pytest --pyargs foo", "pytest tests/missing.py", "pytest tests/*.py", "pytest tests/test_a.py --collect-only",
		"PYTEST_ADDOPTS='-k one' pytest", "python -c 'print(1)'", "cargo test --test integration", "cargo test --lib", "cargo test -p unknown",
		"cargo test --package 'one*'", "cargo test --exclude one", "cargo test -- --exact test_a", "cargo test --manifest-path=src/lib.rs",
		"npm test -- -t one", "npm test -- tests/a.*", "npm test -- unknown-filter", "npm test -- --testPathPattern=tests", "npm test -- --watch",
		"npm test -- --runTestsByPath", "npm build", "npm typecheck", "yarn run-script test", "bun typecheck", "npm run typecheck -- -p src",
		"npm run typecheck -- tests/a.test.ts", "npm run build -- --mode custom", "npm --prefix src test", "echo pytest", "sh -c 'pytest'",
		"go test ./... && pytest", "go test ./...; pytest", "go test ./... > result", "pytest $(pwd)", "pytest $TESTS", "pytest `pwd`", "pytest 'unterminated",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			got, err := Infer(context.Background(), root, ".", command)
			if err != nil {
				t.Fatal(err)
			}
			if got.Known || len(got.Scopes) != 0 {
				t.Fatalf("unsafe coverage for %q: %#v", command, got)
			}
		})
	}
}

func TestManifestIsolationAndFallback(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module parent\n", "child/go.mod": "module child\n", "child/a.go": "package child\n",
		"pyproject.toml": "", "child/pyproject.toml": "", "child/a.py": "",
		"package.json": `{"scripts":{"test":"jest"}}`, "child/package.json": `{"scripts":{"test":"jest"}}`, "child/a.ts": "",
		"no-manifest/tests/test.py": "",
	})
	for _, command := range []string{"go test ./...", "pytest", "npm test"} {
		parent, err := Infer(context.Background(), root, ".", command)
		if err != nil {
			t.Fatal(err)
		}
		child, err := Infer(context.Background(), root, "child", command)
		if err != nil {
			t.Fatal(err)
		}
		if !parent.Known || !child.Known || parent.Family != child.Family || parent.Root == child.Root {
			t.Fatalf("manifest isolation: parent=%#v child=%#v", parent, child)
		}
	}
	bare := fixture(t, map[string]string{"tests/test.py": ""})
	evidence, err := Infer(context.Background(), bare, "tests", "pytest")
	if err != nil || !evidence.Known || evidence.Root != "tests" || !reflect.DeepEqual(evidence.Scopes, []execution.Scope{directory("tests")}) {
		t.Fatalf("fallback = %#v, %v", evidence, err)
	}
}

func TestScriptInspectionIsConservativeAndFresh(t *testing.T) {
	scripts := []string{"custom-runner", "jest && touch marker", "jest; rm file", "vitest", "vitest --watch", "jest $TEST_FILES", "node custom.js", "jest > output"}
	for _, script := range scripts {
		t.Run(script, func(t *testing.T) {
			root := fixture(t, map[string]string{"package.json": `{"scripts":{"test":"` + script + `"}}`})
			evidence, err := Infer(context.Background(), root, ".", "npm test")
			if err != nil || evidence.Known || len(evidence.Scopes) != 0 {
				t.Fatalf("script %q = %#v, %v", script, evidence, err)
			}
		})
	}
	root := fixture(t, map[string]string{"package.json": `{"scripts":{"test":"jest","pretest":"touch file"}}`})
	evidence, err := Infer(context.Background(), root, ".", "npm test")
	if err != nil || evidence.Known {
		t.Fatalf("pretest = %#v, %v", evidence, err)
	}
	put(t, root, "package.json", `{"scripts":{"test":"jest"}}`)
	evidence, err = Infer(context.Background(), root, ".", "npm test")
	if err != nil || !evidence.Known {
		t.Fatalf("updated script = %#v, %v", evidence, err)
	}
	put(t, root, "package.json", `{"scripts":{"build":"vite build"}}`)
	checks, err := Select(context.Background(), root, []string{"package.json"})
	if err != nil || len(checks) != 1 || checks[0].Family != "javascript-build" {
		t.Fatalf("updated manifest = %#v, %v", checks, err)
	}
	evidence, err = Infer(context.Background(), root, ".", "npm test")
	if err != nil || evidence.Known {
		t.Fatalf("deleted test script = %#v, %v", evidence, err)
	}
}

func TestNewFilesAndNestedManifestsDoNotUseCachedEvidence(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": "module root\n", "pkg/a.go": "package pkg\n"})
	before, err := Infer(context.Background(), root, ".", "go test ./pkg")
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "pkg/b_test.go", "package pkg\n")
	after, err := Infer(context.Background(), root, ".", "go test ./pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Scopes) != 1 || len(after.Scopes) != 2 {
		t.Fatalf("cached file list: before=%#v after=%#v", before, after)
	}
	put(t, root, "pkg/go.mod", "module nested\n")
	checks, err := Select(context.Background(), root, []string{"pkg/b_test.go"})
	if err != nil || len(checks) != 1 || checks[0].Root != "pkg" {
		t.Fatalf("cached manifest: %#v, %v", checks, err)
	}
}

func TestPathsQuotesAndBoundaries(t *testing.T) {
	root := fixture(t, map[string]string{"a ' module/go.mod": "module quote\n", "a ' module/a.go": "package quote\n", "python/pyproject.toml": "", "python/test ' one.py": ""})
	got, err := Select(context.Background(), root, []string{"a ' module/a.go"})
	if err != nil || len(got) != 1 || got[0].Command != `cd 'a '"'"' module' && go test ./...` {
		t.Fatalf("quoted recommendation = %#v, %v", got, err)
	}
	e, err := Infer(context.Background(), root, "python", `pytest 'test '"'"' one.py'`)
	if err != nil || !e.Known || !reflect.DeepEqual(e.Scopes, []execution.Scope{exact("python/test ' one.py")}) {
		t.Fatalf("quoted path evidence = %#v, %v", e, err)
	}
	for _, path := range []string{"../escape.go", "/outside/a.go"} {
		if _, err := Select(context.Background(), root, []string{path}); err == nil {
			t.Fatalf("accepted escaping changed path %q", path)
		}
	}
	outside := fixture(t, map[string]string{"go.mod": "module outside\n", "a.go": "package outside\n"})
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(context.Background(), root, []string{"escape/a.go"}); err == nil {
		t.Fatal("accepted escaping symlink")
	}
	if _, err := Infer(context.Background(), root, ".", "go test ./escape/..."); err == nil {
		t.Fatal("accepted escaping command path")
	}
	if _, err := Infer(context.Background(), root, outside, "pytest"); err == nil {
		t.Fatal("accepted escaping cwd")
	}
	if _, err := Infer(context.Background(), root, "python", "pytest ../../escape.py"); err == nil {
		t.Fatal("accepted escaping test path")
	}
	manifestRoot := fixture(t, map[string]string{"a.go": "package root\n"})
	if err := os.Symlink(filepath.Join(outside, "go.mod"), filepath.Join(manifestRoot, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(context.Background(), manifestRoot, []string{"a.go"}); err == nil {
		t.Fatal("accepted escaping manifest")
	}
}

func TestLimitsAndCancellation(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": strings.Repeat("x", 262145), "a.go": "package root\n"})
	if _, err := Select(context.Background(), root, []string{"a.go"}); err == nil {
		t.Fatal("accepted oversized manifest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Select(ctx, root, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select cancellation = %v", err)
	}
	if _, err := Infer(ctx, root, ".", "go test ./..."); !errors.Is(err, context.Canceled) {
		t.Fatalf("Infer cancellation = %v", err)
	}
	if _, err := Select(context.Background(), root, make([]string, 1025)); err == nil {
		t.Fatal("accepted excessive changed paths")
	}
	evidence, err := Infer(context.Background(), root, ".", strings.Repeat("x", 16385))
	if err != nil || evidence.Known {
		t.Fatalf("oversized command = %#v, %v", evidence, err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": "module root\n", "pkg/a.go": "package pkg\n"})
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			checks, err := Select(context.Background(), root, []string{"pkg/a.go"})
			if err != nil || len(checks) != 1 {
				t.Errorf("concurrent Select = %#v, %v", checks, err)
			}
			evidence, err := Infer(context.Background(), root, ".", "go test ./pkg")
			if err != nil || !evidence.Known {
				t.Errorf("concurrent Infer = %#v, %v", evidence, err)
			}
		})
	}
	group.Wait()
}
