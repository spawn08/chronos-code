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
		{"vitest npm", `{"scripts":{"test":"vitest"}}`, "", "npm run test -- --run", "javascript-test", execution.CommandTest},
		{"vitest pnpm", `{"scripts":{"test":"vitest"}}`, "pnpm-lock.yaml", "pnpm run test --run", "javascript-test", execution.CommandTest},
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
			switch tc.name {
			case "go package nonrecursive":
				want.DirectInputDirs = []string{"pkg"}
			case "go default package":
				want.DirectInputDirs = []string{"."}
			}
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
		"go test -count=0 ./...", "go test -count -1 ./...", "go test -count=oops ./...", "go test all", "go test example.org/pkg", "go test ./missing/...", "go test ./... -run TestOne", "go test ./... -tags integration",
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

func TestSymlinkWorkspacePathsAndCwd(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module root\n", "main.go": "package root\n",
		"pkg/a.go": "package pkg\n",
	})
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	aliases := t.TempDir()
	link := filepath.Join(aliases, "workspace")
	if err := os.Symlink(canonical, link); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(aliases, "ancestor")
	if err := os.Symlink(filepath.Dir(canonical), ancestor); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{link, filepath.Join(ancestor, filepath.Base(canonical))} {
		t.Run(workspace, func(t *testing.T) {
			for _, cwd := range []string{"", ".", workspace, canonical} {
				got, err := Infer(context.Background(), workspace, cwd, "go test ./...")
				want := CommandEvidence{Family: "go-test", Root: ".", Scopes: []execution.Scope{directory(".")}, Known: true}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("root cwd %q = %#v, %v, want %#v", cwd, got, err, want)
				}
			}
			for _, cwd := range []string{"pkg", filepath.Join(workspace, "pkg"), filepath.Join(canonical, "pkg")} {
				got, err := Infer(context.Background(), workspace, cwd, "go test ./...")
				want := CommandEvidence{Family: "go-test", Root: ".", Scopes: []execution.Scope{directory("pkg")}, Known: true}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("nested cwd %q = %#v, %v, want %#v", cwd, got, err, want)
				}
			}
			got, err := Select(context.Background(), workspace, []string{filepath.Join(workspace, "pkg/a.go"), filepath.Join(workspace, "pkg/new.go")})
			want := []Check{{Family: "go-test", Root: ".", Command: "go test ./...", Class: execution.CommandTest, Paths: []string{"pkg/a.go", "pkg/new.go"}}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("absolute changed paths = %#v, %v, want %#v", got, err, want)
			}
		})
	}
}

func TestSymlinkWorkspaceRejectsEscapes(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": "module root\n", "a.go": "package root\n"})
	outside := fixture(t, map[string]string{"go.mod": "module outside\n", "a.go": "package outside\n"})
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, link+"-sibling"); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{outside, filepath.Dir(link), link + "-sibling", filepath.Join(link, "escape")} {
		if _, err := Infer(context.Background(), link, cwd, "go test ./..."); err == nil {
			t.Fatalf("accepted escaping cwd %q", cwd)
		}
	}
	for _, path := range []string{filepath.Join(outside, "a.go"), filepath.Join(link+"-sibling", "a.go"), filepath.Join(link, "escape/a.go"), filepath.Join(link, "escape/new.go")} {
		if _, err := Select(context.Background(), link, []string{path}); err == nil {
			t.Fatalf("accepted escaping changed path %q", path)
		}
		if _, err := Infer(context.Background(), link, link, "pytest "+quote(path)); err == nil {
			t.Fatalf("accepted escaping command path %q", path)
		}
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

func TestRecommendedChecksRoundTripToManifestEvidence(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module root\n", "pkg/a.go": "package pkg\n",
		"python/setup.cfg": "[tool:pytest]\n", "python/app.py": "",
		"rust workspace/Cargo.toml": "[package]\nname='crate'\n", "rust workspace/src/lib.rs": "",
		"web/package.json": `{"scripts":{"test":"vitest"}}`, "web/src/a.ts": "",
	})
	checks, err := Select(context.Background(), root, []string{"go.mod", "pkg/a.go", "python/setup.cfg", "python/app.py", "rust workspace/Cargo.toml", "rust workspace/src/lib.rs", "web/package.json", "web/src/a.ts"})
	if err != nil || len(checks) != 4 {
		t.Fatalf("Select = %#v, %v", checks, err)
	}
	for _, check := range checks {
		command := check.Command
		if check.Root != "." {
			command = strings.TrimPrefix(command, "cd "+quote(check.Root)+" && ")
		}
		evidence, err := Infer(context.Background(), root, check.Root, command)
		if err != nil || !evidence.Known || evidence.Family != check.Family || evidence.Root != check.Root {
			t.Fatalf("roundtrip %v = %#v, %v", check, evidence, err)
		}
		for _, path := range check.Paths {
			if !execution.ScopesOverlap(evidence.Scopes, []execution.Scope{exact(path)}) {
				t.Errorf("recommendation %q does not cover %q: %#v", command, path, evidence)
			}
		}
	}
}

func TestTargetedTestsDoNotCertifyProductionOrOtherTests(t *testing.T) {
	root := fixture(t, map[string]string{
		"pyproject.toml": "", "tests/test_one.py": "", "tests/test_two.py": "", "src/app.py": "",
		"package.json": `{"scripts":{"test":"jest tests/one.test.ts"}}`, "tests/one.test.ts": "", "tests/two.test.ts": "", "src/app.ts": "",
	})
	for _, command := range []string{"pytest tests/test_one.py::test_one", "npm test"} {
		evidence, err := Infer(context.Background(), root, ".", command)
		if err != nil || !evidence.Known {
			t.Fatalf("Infer = %#v, %v", evidence, err)
		}
		for _, path := range []string{"src/app.py", "src/app.ts", "tests/test_two.py", "tests/two.test.ts"} {
			if execution.ScopesOverlap(evidence.Scopes, []execution.Scope{exact(path)}) {
				t.Errorf("targeted command %q certifies unrelated %q", command, path)
			}
		}
	}
}

func TestNonRegularAndTotalReadLimits(t *testing.T) {
	root := fixture(t, map[string]string{"a.go": "package root\n"})
	if err := os.Mkdir(filepath.Join(root, "go.mod"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(context.Background(), root, []string{"a.go"}); err == nil {
		t.Fatal("accepted nonregular manifest")
	}
	root = fixture(t, map[string]string{"go.mod": strings.Repeat("x", 262144), "a.go": "package root\n"})
	changed := make([]string, 17)
	for i := range changed {
		changed[i] = "a.go"
	}
	if _, err := Select(context.Background(), root, changed); err == nil {
		t.Fatal("accepted excessive total reads")
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

func TestWordsLiteralShellQuoting(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{`pytest "tests/test\ one.py"`, []string{"pytest", `tests/test\ one.py`}},
		{`pytest "tests/test\\one.py"`, []string{"pytest", `tests/test\one.py`}},
		{`pytest "tests/test\"one.py"`, []string{"pytest", `tests/test"one.py`}},
		{`pytest tests/test\ one.py`, []string{"pytest", "tests/test one.py"}},
		{`pytest 'tests/test\one.py'`, []string{"pytest", `tests/test\one.py`}},
		{`pytest tests/"test one".py`, []string{"pytest", "tests/test one.py"}},
		{"pytest tests/test\u00a0one.py", []string{"pytest", "tests/test\u00a0one.py"}},
		{`CI="1" go test "-count=1" ./...`, []string{"CI=1", "go", "test", "-count=1", "./..."}},
		{`pytest ''`, []string{"pytest", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got, ok := words(tc.command, 16384)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("words(%q) = %#v, %v, want %#v", tc.command, got, ok, tc.want)
			}
		})
	}
	for _, command := range []string{`pytest "unterminated`, `pytest trailing\`, `go test # ./pkg`, `pytest ~/tests`, `pytest tests/{one,two}.py`, `"CI=1" go test`, `C"I"=1 go test`, `CI\=1 go test`, `C\I=1 go test`} {
		if got, ok := words(command, 16384); ok {
			t.Errorf("accepted ambiguous shell command %q: %#v", command, got)
		}
	}
}

func TestInferSelectorsEnvironmentAndQuoting(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module root\n", "pkg/a.go": "package pkg\n", "pkg/b_test.go": "package pkg\n", "other/a.go": "package other\n",
		"pyproject.toml": "", "tests/test one.py": "", "tests/test\\ one.py": "", "tests/test\\one.py": "", "tests/test\"one.py": "", "tests/test\u00a0one.py": "",
		"package.json": `{"scripts":{"test":"jest"}}`, "tests/one.test.ts": "", "tests/two.test.ts": "",
	})
	cases := []struct {
		command string
		scopes  []execution.Scope
	}{
		{`go test ./pkg ./other`, []execution.Scope{exact("other/a.go"), exact("pkg/a.go"), exact("pkg/b_test.go")}},
		{`go test ./pkg/a.go ./pkg/b_test.go`, []execution.Scope{exact("pkg/a.go"), exact("pkg/b_test.go")}},
		{`env CI="1" NO_COLOR=1 TERM=dumb FORCE_COLOR=0 go test ./pkg`, []execution.Scope{exact("pkg/a.go"), exact("pkg/b_test.go")}},
		{`CI=1 time -p env NO_COLOR=1 go test ./pkg`, nil},
		{`time -p env CI='1' pytest "tests/test one.py"`, []execution.Scope{exact("tests/test one.py")}},
		{`pytest "tests/test\ one.py"`, []execution.Scope{exact(`tests/test\ one.py`)}},
		{`pytest "tests/test\\one.py"`, []execution.Scope{exact(`tests/test\one.py`)}},
		{`pytest "tests/test\"one.py"`, []execution.Scope{exact(`tests/test"one.py`)}},
		{`pytest tests/test\ one.py`, []execution.Scope{exact("tests/test one.py")}},
		{"pytest tests/test\u00a0one.py", []execution.Scope{exact("tests/test\u00a0one.py")}},
		{`pytest 'tests/test one.py::test_case[a*]'`, []execution.Scope{exact("tests/test one.py")}},
		{`npm test -- --runTestsByPath tests/one.test.ts tests/two.test.ts`, []execution.Scope{exact("tests/one.test.ts"), exact("tests/two.test.ts")}},
		{`go test ./pkg ./other/a.go`, nil},
		{`go test ./pkg -run=TestOne`, nil},
		{`GOFLAGS=-short go test ./pkg`, nil},
		{`env GOOS=linux go test ./pkg`, nil},
		{`env -i go test ./pkg`, nil},
		{`env -C pkg go test`, nil},
		{`"CI=1" go test ./pkg`, nil},
		{`CI\=1 go test ./pkg`, nil},
		{`PYTEST_ADDOPTS=-x pytest`, nil},
		{`NODE_OPTIONS=--require=hook.js npm test`, nil},
		{`pytest tests/test\ one.py -k case`, nil},
		{"go test\u00a0./pkg", nil},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got, err := Infer(context.Background(), root, ".", tc.command)
			if err != nil || got.Known != (len(tc.scopes) > 0) || !reflect.DeepEqual(got.Scopes, tc.scopes) {
				t.Fatalf("Infer(%q) = %#v, %v, want scopes %#v", tc.command, got, err, tc.scopes)
			}
		})
	}
}

func TestInferDoubleQuotedBackslashDoesNotCertifyDecodedPath(t *testing.T) {
	root := fixture(t, map[string]string{"pyproject.toml": "", "tests/test one.py": ""})
	got, err := Infer(context.Background(), root, ".", `pytest "tests/test\ one.py"`)
	if err != nil || got.Known || len(got.Scopes) != 0 {
		t.Fatalf("backslash incorrectly removed: %#v, %v", got, err)
	}
}

func TestInferNonrecursiveGoRescansDeletedSiblings(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": "module root\n", "pkg/a.go": "package pkg\n", "pkg/b_test.go": "package pkg\n", "pkg/deep/c.go": "package deep\n"})
	before, err := Infer(context.Background(), root, ".", "go test ./pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "pkg/b_test.go")); err != nil {
		t.Fatal(err)
	}
	after, err := Infer(context.Background(), root, ".", "go test ./pkg")
	if err != nil || !reflect.DeepEqual(before.Scopes, []execution.Scope{exact("pkg/a.go"), exact("pkg/b_test.go")}) || !reflect.DeepEqual(after.Scopes, []execution.Scope{exact("pkg/a.go")}) {
		t.Fatalf("deleted sibling rescan: before=%#v after=%#v, %v", before, after, err)
	}
}

func TestInferInspectedScriptEnvironmentAndQuoting(t *testing.T) {
	cases := []struct {
		pkg    string
		known  bool
		scopes []execution.Scope
	}{
		{`{"scripts":{"test":"CI=1 jest \"tests/test one.ts\""}}`, true, []execution.Scope{exact("tests/test one.ts")}},
		{`{"scripts":{"test":"jest \"tests/test\\ one.ts\""}}`, false, nil},
		{`{"scripts":{"test":"NODE_OPTIONS=--require=hook.js jest"}}`, false, nil},
		{`{"scripts":{"test":"\"CI=1\" jest"}}`, false, nil},
		{`{"scripts":{"test":"jest --testNamePattern=one"}}`, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.pkg, func(t *testing.T) {
			root := fixture(t, map[string]string{"package.json": tc.pkg, "tests/test one.ts": ""})
			got, err := Infer(context.Background(), root, ".", "npm test")
			if err != nil || got.Known != tc.known || !reflect.DeepEqual(got.Scopes, tc.scopes) {
				t.Fatalf("script evidence = %#v, %v, want known=%v scopes=%#v", got, err, tc.known, tc.scopes)
			}
		})
	}
}
