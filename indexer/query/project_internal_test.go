package query

import (
	"fmt"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/extract/manifest"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func dep(name, p string) facts.Import {
	return facts.Import{Kind: facts.ImportDepend, Name: name, Path: p}
}
func root(name, p string) facts.Import {
	return facts.Import{Kind: facts.ImportRoot, Name: name, Path: p}
}

// TestProjectDepClosure maps dependencies by name and path for every build
// system and follows them transitively.
func TestProjectDepClosure(t *testing.T) {
	p := &project{byDir: map[string][]*manifestFile{}, byName: map[string]map[string]string{},
		gradle: map[string]string{}, swiftTargets: map[string]string{}, declared: map[string]bool{}}
	for _, mf := range []*manifestFile{
		{kind: manifest.KindGradle, path: "settings.gradle", imports: []facts.Import{root(":libs:json", "third/json")}},
		{kind: manifest.KindGradle, path: "app/build.gradle", imports: []facts.Import{dep(":core", ""), dep(":libs:json", "")}},
		{kind: manifest.KindGradle, path: "core/build.gradle.kts"},
		{kind: manifest.KindGradle, path: "third/json/build.gradle"},
		{kind: manifest.KindMaven, path: "svc/pom.xml", name: "svc", imports: []facts.Import{dep("model", "")}},
		{kind: manifest.KindMaven, path: "model/pom.xml", name: "model", imports: []facts.Import{dep("guava", "")}},
		{kind: manifest.KindCargo, path: "cli/Cargo.toml", name: "cli", imports: []facts.Import{dep("acme_core", "")}},
		{kind: manifest.KindCargo, path: "crates/core/Cargo.toml", name: "acme-core", imports: []facts.Import{dep("util", "crates/util")}},
		{kind: manifest.KindCargo, path: "crates/util/Cargo.toml", name: "util"},
		{kind: manifest.KindCSProj, path: "src/App/App.csproj", name: "App", imports: []facts.Import{dep("Core", "src/Core")}},
		{kind: manifest.KindCSProj, path: "src/Core/Core.csproj", name: "Core"},
		{kind: manifest.KindBazel, path: "x/BUILD", imports: []facts.Import{dep("", "y/z")}},
		{kind: manifest.KindBazel, path: "y/BUILD"},
		{kind: manifest.KindTSConfig, path: "web/tsconfig.json"},
	} {
		mf.dir = dirOf(mf.path)
		p.add(mf)
	}
	for unit, want := range map[string]string{
		"app":         "[app core third/json]",
		"svc":         "[model svc]",
		"cli":         "[cli crates/core crates/util]",
		"src/App":     "[src/App src/Core]",
		"x":           "[x y]", // y/z has no BUILD: its unit is y
		"web":         "[]",    // a tsconfig declares no dependencies
		"crates/util": "[crates/util]",
	} {
		got, _ := p.depClosure(unit)
		if fmt.Sprint(got) != want {
			t.Errorf("depClosure(%s) = %v, want %s", unit, got, want)
		}
	}
	if u, ok := p.buildUnit("app/src/main/java/org"); !ok || u != "app" {
		t.Errorf("buildUnit = %q %v", u, ok)
	}
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"UsersController": "users_controller", "HTTPClient": "http_client", "User": "user", "V2Api": "v2_api",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestMatchAlias(t *testing.T) {
	for _, c := range []struct{ pattern, target, spec, want string }{
		{"@app/*", "src/app/*", "@app/x/y", "src/app/x/y"},
		{"@lib", "lib/index.ts", "@lib", "lib/index.ts"},
		{"@lib", "lib/index.ts", "@lib/x", ""},
		{"./*", "src/*.ts", "./utils", "src/utils.ts"},
	} {
		got, ok := matchAlias(c.pattern, c.target, c.spec)
		if !ok {
			got = ""
		}
		if got != c.want {
			t.Errorf("matchAlias(%s, %s, %s) = %q, want %q", c.pattern, c.target, c.spec, got, c.want)
		}
	}
}
