package query_test

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/indexer/query"
)

// Language resolvers from project manifests (M7). Each fixture has a decoy
// declaration of the same name elsewhere, so a correct answer cannot come
// from name matching.

// targetFiles returns the files of the targets of each outgoing call of
// from, keyed name@line.
func targetFiles(t *testing.T, v *query.View, from string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range outgoingSymbols(t, v, from) {
		key := c.name + "@" + itoa(c.line)
		out[key] = strings.Join(c.files, ",")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func expectTargets(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s targets %q, want %q (all: %v)", k, got[k], w, got)
		}
	}
}

func TestResolveTSConfigPaths(t *testing.T) {
	v := newFixture(t, map[string]string{
		"tsconfig.base.json": `{
  // shared settings
  "compilerOptions": {"paths": {"@lib/*": ["libs/*"]},},
}`,
		"app/tsconfig.json":            `{"extends": "../tsconfig.base.json", "compilerOptions": {"baseUrl": "./src"}}`,
		"libs/util.ts":                 "export function helper(): number { return 1; }\n",
		"app/src/components/button.ts": "export function render(): void {}\n",
		"decoy/util.ts":                "export function helper(): number { return 2; }\n",
		"decoy/components/button.ts":   "export function render(): void {}\n",
		"app/src/main.ts": `import { helper } from '@lib/util';
import { render } from 'components/button';

export function main(): void {
    helper();
    render();
}
`,
	}).view(t)
	expect(t, resolved(t, v, "main"), map[string]string{
		"helper@5": "import_resolved helper",
		"render@6": "import_resolved render",
	})
	expectTargets(t, targetFiles(t, v, "main"), map[string]string{
		"helper@5": "libs/util.ts",
		"render@6": "app/src/components/button.ts",
	})
}

func TestResolveNPMWorkspaceAndBarrels(t *testing.T) {
	v := newFixture(t, map[string]string{
		"package.json":                      `{"name": "root", "private": true, "workspaces": ["packages/*", "apps/*"]}`,
		"packages/core/package.json":        `{"name": "@acme/core", "main": "dist/index.js"}`,
		"packages/core/src/index.ts":        "export * from './lib/parser';\nexport { tokenize as lex } from './lib/lexer';\n",
		"packages/core/src/lib/parser.ts":   "export function parse(s: string): number { return 0; }\n",
		"packages/core/src/lib/lexer.ts":    "export function tokenize(s: string): string[] { return []; }\n",
		"packages/core/src/format/index.ts": "export function pretty(): void {}\n",
		"apps/other/parse.ts":               "export function parse(s: string): number { return 1; }\nexport function lex(): void {}\nexport function pretty(): void {}\n",
		"apps/web/package.json":             `{"name": "web", "dependencies": {"@acme/core": "workspace:*"}}`,
		"apps/web/src/main.ts": `import { parse, lex } from '@acme/core';
import * as fmt from '@acme/core/format';

export function main(): void {
    parse("x");
    lex("y");
    fmt.pretty();
}
`,
	}).view(t)
	expect(t, resolved(t, v, "main"), map[string]string{
		"parse@5":  "import_resolved parse",
		"lex@6":    "import_resolved tokenize",
		"pretty@7": "import_resolved pretty",
	})
	expectTargets(t, targetFiles(t, v, "main"), map[string]string{
		"parse@5":  "packages/core/src/lib/parser.ts",
		"pretty@7": "packages/core/src/format/index.ts",
	})
}

func TestResolvePythonInitReexports(t *testing.T) {
	v := newFixture(t, map[string]string{
		"pkg/__init__.py":      "from .impl.core import Engine\n",
		"pkg/impl/__init__.py": "",
		"pkg/impl/core.py":     "class Engine:\n    pass\n",
		"other/engine.py":      "class Engine:\n    pass\n",
		"app/main.py":          "from pkg import Engine\n\n\ndef run():\n    Engine()\n",
	}).view(t)
	expect(t, resolved(t, v, "run"), map[string]string{"Engine@5": "import_resolved Engine"})
	expectTargets(t, targetFiles(t, v, "run"), map[string]string{"Engine@5": "pkg/impl/core.py"})
}

func TestResolveRustWorkspaceCrates(t *testing.T) {
	v := newFixture(t, map[string]string{
		"Cargo.toml":                    "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/core/Cargo.toml":        "[package]\nname = \"acme-core\"\n",
		"crates/core/src/lib.rs":        "pub mod model;\npub use self::model::item::Item;\n",
		"crates/core/src/model/mod.rs":  "pub mod item;\n",
		"crates/core/src/model/item.rs": "pub struct Item;\n\nimpl Item {\n    pub fn new() -> Self { Item }\n}\n",
		"crates/other/Cargo.toml":       "[package]\nname = \"other\"\n",
		"crates/other/src/lib.rs":       "pub struct Item;\n\nimpl Item {\n    pub fn new() -> Self { Item }\n}\n",
		"crates/cli/Cargo.toml":         "[package]\nname = \"cli\"\n\n[dependencies]\nacme-core = { path = \"../core\" }\n",
		"crates/cli/src/main.rs":        "use acme_core::Item;\n\nfn main() {\n    let i = Item::new();\n}\n",
	}).view(t)
	expect(t, resolved(t, v, "main"), map[string]string{"new@4": "import_resolved Item.new"})
	expectTargets(t, targetFiles(t, v, "main"), map[string]string{"new@4": "crates/core/src/model/item.rs"})
}

func TestResolvePHPComposerPSR4(t *testing.T) {
	v := newFixture(t, map[string]string{
		"composer.json":        `{"autoload": {"psr-4": {"App\\": "src/"}}}`,
		"src/Models/User.php":  "<?php\nnamespace App\\Models;\n\nclass User {\n    public static function find($id) { return null; }\n}\n",
		"lib/User.php":         "<?php\nnamespace Legacy;\n\nclass User {\n    public static function find($id) { return null; }\n}\n",
		"src/Http/Handler.php": "<?php\nnamespace App\\Http;\n\nuse App\\Models\\User;\n\nfunction handle() {\n    return new User();\n}\n",
	}).view(t)
	expect(t, resolved(t, v, "handle"), map[string]string{"User@7": "import_resolved User"})
	expectTargets(t, targetFiles(t, v, "handle"), map[string]string{"User@7": "src/Models/User.php"})
}

func TestResolveCIncludeRoots(t *testing.T) {
	f := newFixture(t, map[string]string{
		"include/acme/api.h": "int acme_run(int n);\n",
		"src/api.c":          "#include \"acme/api.h\"\nint acme_run(int n) { return n; }\n",
		"other/acme/api.h":   "int acme_run(int n);\n",
		"other/api.c":        "int acme_run(int n) { return 0; }\n",
		"tools/main.c":       "#include \"acme/api.h\"\n\nint main(void) {\n    return acme_run(1);\n}\n",
	})
	write(t, f.root, "build/compile_commands.json",
		`[{"directory": "`+f.root+`/build", "command": "cc -I../include -c ../tools/main.c", "file": "../tools/main.c"}]`)
	if _, err := f.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := f.view(t)
	got := targetFiles(t, v, "main")
	// The include resolves to include/acme/api.h, and the call to its
	// definition next to it rather than other/'s.
	if !strings.Contains(got["acme_run@4"], "include/acme/api.h") && !strings.Contains(got["acme_run@4"], "src/api.c") ||
		strings.Contains(got["acme_run@4"], "other/") {
		t.Fatalf("acme_run@4 targets %q", got["acme_run@4"])
	}
}

func TestResolveDartPubspecPackages(t *testing.T) {
	v := newFixture(t, map[string]string{
		"pkgs/core/pubspec.yaml":      "name: core\n",
		"pkgs/core/lib/src/util.dart": "int helper() => 1;\n",
		"lib/src/util.dart":           "int helper() => 2;\n",
		"pkgs/app/pubspec.yaml":       "name: app\ndependencies:\n  core:\n    path: ../core\n",
		"pkgs/app/lib/main.dart":      "import 'package:core/src/util.dart';\n\nvoid start() {\n  helper();\n}\n",
	}).view(t)
	expectTargets(t, targetFiles(t, v, "start"), map[string]string{"helper@4": "pkgs/core/lib/src/util.dart"})
}

func TestResolveSwiftPMTargets(t *testing.T) {
	v := newFixture(t, map[string]string{
		"Package.swift": `// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Acme",
    targets: [
        .target(name: "Core"),
        .executableTarget(name: "App", dependencies: ["Core"]),
    ]
)
`,
		"Sources/Core/Model/Item.swift": "struct Item {\n    func save() {}\n}\n",
		"Sources/Core/Store.swift":      "func load() -> Item {\n    return Item()\n}\n",
		"Sources/App/Item.swift":        "struct Item {\n    func save() {}\n}\n",
	}).view(t)
	expectTargets(t, targetFiles(t, v, "load"), map[string]string{"Item@2": "Sources/Core/Model/Item.swift"})
}

func TestResolveDeclaredPackages(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/x/Util.java":     "package org.x;\n\npublic class Util {\n}\n",
		"src/main/java/org/y/Util.java":     "package org.y;\n\npublic class Util {\n}\n",
		"src/test/java/org/x/UtilTest.java": "package org.x;\n\nclass UtilTest {\n    void run() {\n        new Util();\n    }\n}\n",
		// Kotlin files need not mirror their package in directories.
		"src/main/kotlin/Api.kt":      "package org.k.api\n\nclass Client\n",
		"src/main/kotlin/Decoy.kt":    "package org.k.decoy\n\nclass Client\n",
		"src/main/kotlin/app/Main.kt": "package org.k.app\n\nimport org.k.api.Client\n\nfun start() {\n    Client()\n}\n",
	}).view(t)
	expect(t, resolved(t, v, "UtilTest.run"), map[string]string{"Util@5": "import_resolved Util"})
	expectTargets(t, targetFiles(t, v, "UtilTest.run"), map[string]string{"Util@5": "src/main/java/org/x/Util.java"})
	expect(t, resolved(t, v, "start"), map[string]string{"Client@6": "import_resolved Client"})
	expectTargets(t, targetFiles(t, v, "start"), map[string]string{"Client@6": "src/main/kotlin/Api.kt"})
}

func TestResolveBuildGraphRestriction(t *testing.T) {
	v := newFixture(t, map[string]string{
		// Bazel: app depends on //lib/a only.
		"lib/a/BUILD": "py_library(name = \"a\", srcs = [\"a.py\"])\n",
		"lib/a/a.py":  "class Widget:\n    def process(self):\n        pass\n",
		"lib/b/BUILD": "py_library(name = \"b\", srcs = [\"b.py\"])\n",
		"lib/b/b.py":  "class Gadget:\n    def process(self):\n        pass\n",
		"app/BUILD":   "py_binary(name = \"app\", srcs = [\"main.py\"], deps = [\"//lib/a\"])\n",
		"app/main.py": "def go(x):\n    x.process()\n",
		// npm: web depends on package a only.
		"packages/a/package.json": `{"name": "a"}`,
		"packages/a/src/view.ts":  "export class View {\n    render(): void {}\n}\n",
		"packages/b/package.json": `{"name": "b"}`,
		"packages/b/src/view.ts":  "export class Panel {\n    render(): void {}\n}\n",
		"apps/web/package.json":   `{"name": "web", "dependencies": {"a": "*"}}`,
		"apps/web/src/main.ts":    "export function show(x: any): void {\n    x.render();\n}\n",
	}).view(t)
	expect(t, resolved(t, v, "go"), map[string]string{"process@2": "name_matched Widget.process"})
	expect(t, resolved(t, v, "show"), map[string]string{"render@2": "name_matched View.render"})
}
