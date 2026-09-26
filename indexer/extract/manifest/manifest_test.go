package manifest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/indexer/facts"
)

func render(f *facts.File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s name=%q", f.Lang, f.PkgName)
	if f.ParseErr != "" {
		fmt.Fprintf(&b, " err")
	}
	kinds := map[uint8]string{facts.ImportDepend: "dep", facts.ImportAlias: "alias", facts.ImportRoot: "root", facts.ImportModule: "extends"}
	for _, imp := range f.Imports {
		fmt.Fprintf(&b, "; %s %s=%s", kinds[imp.Kind], imp.Name, imp.Path)
	}
	for _, e := range f.Exports {
		fmt.Fprintf(&b, "; export %s=%s", e.Name, e.Source)
	}
	return b.String()
}

func TestExtract(t *testing.T) {
	for _, c := range []struct {
		path, src, want string
	}{
		{"web/package.json", `{"name": "@acme/web", "main": "dist/index.js", "types": "./src/index.ts",
			"exports": {".": {"types": "./src/index.ts", "default": "./dist/index.js"}, "./utils": "./src/utils/index.ts"},
			"dependencies": {"@acme/core": "workspace:*", "react": "^18"}, "devDependencies": {"vitest": "1"}}`,
			`manifest:npm name="@acme/web"; dep @acme/core=; dep react=; dep vitest=; export .=web/src/index.ts; export ./utils=web/src/utils/index.ts; export .=web/dist/index.js`},
		{"package.json", `{"name": "root", "private": true, "workspaces": ["packages/*", "apps/web"]}`,
			`manifest:npm name="root"; root =packages/*; root =apps/web`},
		{"package.json", `{"workspaces": {"packages": ["libs/*"]}}`, `manifest:npm name=""; root =libs/*`},
		{"pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n  - '!**/test/**'\n", `manifest:npm name=""; root =packages/*`},
		{"app/tsconfig.json", `{
  // comment
  "extends": "../tsconfig.base",
  "compilerOptions": {
    "baseUrl": "./src", /* block */
    "paths": {"@app/*": ["app/*"], "@lib": ["../../lib/index.ts"], "~/*": ["./*"],},
  },
}`, `manifest:tsconfig name=""; extends =tsconfig.base.json; root =app/src; alias @app/*=app/src/app/*; alias @lib=lib/index.ts; alias ~/*=app/src/*`},
		{"tsconfig.json", `{"compilerOptions": {"paths": {"@/*": ["src/*"]}}}`, `manifest:tsconfig name=""; alias @/*=src/*`},
		{"crates/cli/Cargo.toml", `[package]
name = "acme-cli" # the binary

[dependencies]
acme-core = { path = "../core", version = "0.1" }
serde = "1"
[dependencies.acme_util]
path = "../util"

[dev-dependencies]
tempfile = "3"
`, `manifest:cargo name="acme-cli"; dep acme_core=crates/core; dep serde=; dep acme_util=crates/util; dep tempfile=`},
		{"Cargo.toml", "[workspace]\nmembers = [\n  \"crates/*\",\n  \"tools/gen\",\n]\n", `manifest:cargo name=""; root =crates/*; root =tools/gen`},
		{"composer.json", `{"name": "acme/app", "autoload": {"psr-4": {"App\\": "src/", "Lib\\": ["lib/", "legacy/"]}}, "autoload-dev": {"psr-4": {"Tests\\": "tests/"}}}`,
			`manifest:composer name="acme/app"; alias App\=src; alias Lib\=lib; alias Lib\=legacy; alias Tests\=tests`},
		{"core/pom.xml", `<project><artifactId>core</artifactId><modules><module>../api</module></modules>
<dependencies><dependency><groupId>g</groupId><artifactId>util</artifactId></dependency></dependencies></project>`,
			`manifest:maven name="core"; root =api; dep util=`},
		{"app/build.gradle.kts", `dependencies { implementation(project(":core")); api(project(path = ":lib:json")) }`,
			`manifest:gradle name=""; dep :core=; dep :lib:json=`},
		{"settings.gradle", "rootProject.name = 'acme'\ninclude ':core', ':lib:json'\ninclude(\"app\")\n",
			`manifest:gradle name="acme"; root :core=core; root :lib:json=lib/json; root :app=app`},
		{"svc/api/BUILD.bazel", `java_library(
    name = "api",
    srcs = glob(["*.java"]),
    deps = [
        ":model",
        "//svc/core:core",
        "//lib/json",
        "@maven//:guava",
    ],
)`, `manifest:bazel name=""; dep =svc/core; dep =lib/json`},
		{"src/App/App.csproj", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><AssemblyName>Acme.App</AssemblyName></PropertyGroup>
<ItemGroup><ProjectReference Include="..\Core\Core.csproj" /><PackageReference Include="X" /></ItemGroup></Project>`,
			`manifest:csproj name="Acme.App"; dep Core=src/Core`},
		{"pkgs/app/pubspec.yaml", "name: app\ndependencies:\n  core:\n    path: ../core\n  http: ^1.0\n",
			`manifest:pubspec name="app"; dep core=pkgs/core; dep http=`},
		{"Package.swift", `let package = Package(
    name: "Acme",
    targets: [
        .target(name: "Core", dependencies: []),
        .target(name: "Net", dependencies: ["Core"], path: "Sources/Networking"),
        .testTarget(name: "CoreTests", dependencies: ["Core"]),
    ]
)`, `manifest:swiftpm name="Acme"; root Core=Sources/Core; root Net=Sources/Networking; root CoreTests=Tests/CoreTests`},
		{"pyproject.toml", "[project]\nname = \"acme\"\n\n[tool.setuptools.packages.find]\nwhere = [\"src\"]\n",
			`manifest:pyproject name="acme"; root =src`},
		{"build/compile_commands.json", `[{"directory": "/repo/build", "command": "cc -I../include -I /repo/third_party/x -isystem/usr/include -c ../src/a.c", "file": "../src/a.c"},
{"directory": "/repo", "arguments": ["cc", "-Iinclude", "-iquote", "src", "-c", "src/b.c"], "file": "src/b.c"}]`,
			`manifest:compdb name=""; root =include; root =third_party/x; root =src`},
		{"package.json", `{"name": `, `manifest:npm name="" err`},
	} {
		f := &facts.File{Path: c.path}
		Extract(f, []byte(c.src), "/repo")
		if got := render(f); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.path, got, c.want)
		}
	}
}

func TestKind(t *testing.T) {
	for p, want := range map[string]string{
		"a/package.json": KindNPM, "BUILD": KindBazel, "x/BUILD.bazel": KindBazel, "a/B.csproj": KindCSProj,
		".csproj": "", "a/main.go": "", "settings.gradle.kts": KindGradle, "Package.swift": KindSwiftPM,
	} {
		if got := Kind(p); got != want {
			t.Errorf("Kind(%q) = %q, want %q", p, got, want)
		}
	}
}
