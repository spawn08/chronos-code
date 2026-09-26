// Package manifest extracts project facts from build and package manifests
// (package.json, tsconfig.json, Cargo.toml, pom.xml, BUILD, ...), so the
// language resolvers can read module names, path aliases, source roots and
// build-unit dependencies from the index like any other facts. Each file's
// facts come from that file alone; relative paths are resolved against its
// directory and recorded root-relative.
//
// A manifest is a facts.File whose Lang is one of the Kind constants, with:
//
//	PkgName   the declared name (npm package, crate, artifactId, assembly)
//	Imports   facts.ImportDepend  dependencies on other build units
//	          facts.ImportAlias   spec pattern -> path pattern (tsconfig
//	                              paths, composer PSR-4)
//	          facts.ImportRoot    source roots, include directories and
//	                              member globs (baseUrl, -I, workspaces)
//	          facts.ImportModule  a configuration it extends (tsconfig)
//	Exports   package entry points: Name is the subpath (".", "./utils"),
//	          Source the root-relative target (package.json exports, main)
package manifest

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// Manifest kinds, recorded as facts.File.Lang.
const (
	KindNPM       = "manifest:npm"       // package.json, pnpm-workspace.yaml
	KindTSConfig  = "manifest:tsconfig"  // tsconfig.json, jsconfig.json
	KindCargo     = "manifest:cargo"     // Cargo.toml
	KindComposer  = "manifest:composer"  // composer.json
	KindMaven     = "manifest:maven"     // pom.xml
	KindGradle    = "manifest:gradle"    // build.gradle(.kts), settings.gradle(.kts)
	KindBazel     = "manifest:bazel"     // BUILD, BUILD.bazel, BUCK, TARGETS
	KindCSProj    = "manifest:csproj"    // *.csproj
	KindCompDB    = "manifest:compdb"    // compile_commands.json
	KindPubspec   = "manifest:pubspec"   // pubspec.yaml
	KindSwiftPM   = "manifest:swiftpm"   // Package.swift
	KindPyProject = "manifest:pyproject" // pyproject.toml
)

var byName = map[string]string{
	"package.json": KindNPM, "pnpm-workspace.yaml": KindNPM,
	"tsconfig.json": KindTSConfig, "jsconfig.json": KindTSConfig,
	"Cargo.toml":    KindCargo,
	"composer.json": KindComposer,
	"pom.xml":       KindMaven,
	"build.gradle":  KindGradle, "build.gradle.kts": KindGradle,
	"settings.gradle": KindGradle, "settings.gradle.kts": KindGradle,
	"BUILD": KindBazel, "BUILD.bazel": KindBazel, "BUCK": KindBazel, "TARGETS": KindBazel,
	"compile_commands.json": KindCompDB,
	"pubspec.yaml":          KindPubspec,
	"Package.swift":         KindSwiftPM,
	"pyproject.toml":        KindPyProject,
}

// Kind returns the manifest kind of a root-relative path, or "".
func Kind(rel string) string {
	base := path.Base(rel)
	if k, ok := byName[base]; ok {
		return k
	}
	if strings.HasSuffix(base, ".csproj") && len(base) > len(".csproj") {
		return KindCSProj
	}
	// tsconfig.base.json, tsconfig.app.json: configurations others extend
	if (strings.HasPrefix(base, "tsconfig.") || strings.HasPrefix(base, "jsconfig.")) && strings.HasSuffix(base, ".json") {
		return KindTSConfig
	}
	return ""
}

// IsManifest reports whether lang is a manifest kind.
func IsManifest(lang string) bool { return strings.HasPrefix(lang, "manifest:") }

// Extract fills f (Path set) from the manifest's content. root is the
// absolute repository root, for manifests that record absolute paths
// (compile_commands.json). A manifest that cannot be read records the
// error in f.ParseErr.
func Extract(f *facts.File, src []byte, root string) {
	f.Lang = Kind(f.Path)
	x := &extractor{f: f, dir: path.Dir(f.Path), root: root}
	var err error
	switch f.Lang {
	case KindNPM:
		if path.Base(f.Path) == "pnpm-workspace.yaml" {
			err = x.pnpm(src)
		} else {
			err = x.npm(src)
		}
	case KindTSConfig:
		err = x.tsconfig(src)
	case KindCargo:
		x.cargo(src)
	case KindComposer:
		err = x.composer(src)
	case KindMaven:
		err = x.maven(src)
	case KindGradle:
		x.gradle(src)
	case KindBazel:
		x.bazel(src)
	case KindCSProj:
		err = x.csproj(src)
	case KindCompDB:
		err = x.compdb(src)
	case KindPubspec:
		err = x.pubspec(src)
	case KindSwiftPM:
		x.swiftpm(src)
	case KindPyProject:
		x.pyproject(src)
	}
	if err != nil {
		f.ParseErr = err.Error()
	}
}

type extractor struct {
	f         *facts.File
	dir, root string
}

// rel resolves p against the manifest's directory; "" when it leaves the
// repository or is absolute.
func (x *extractor) rel(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" || path.IsAbs(p) {
		return ""
	}
	out := path.Join(x.dir, p)
	if out == ".." || strings.HasPrefix(out, "../") {
		return ""
	}
	return out
}

func (x *extractor) add(kind uint8, name, p string) {
	imp := facts.Import{Kind: kind, Name: name, Path: p, Line: 1}
	if !slices.ContainsFunc(x.f.Imports, func(o facts.Import) bool {
		return o.Kind == imp.Kind && o.Name == imp.Name && o.Path == imp.Path
	}) {
		x.f.Imports = append(x.f.Imports, imp)
	}
}

// --- JavaScript and TypeScript ---

func (x *extractor) npm(src []byte) error {
	var pkg struct {
		Name       string          `json:"name"`
		Main       string          `json:"main"`
		Module     string          `json:"module"`
		Types      string          `json:"types"`
		Typings    string          `json:"typings"`
		Source     string          `json:"source"`
		Exports    json.RawMessage `json:"exports"`
		Workspaces json.RawMessage `json:"workspaces"`
		Deps       map[string]any  `json:"dependencies"`
		DevDeps    map[string]any  `json:"devDependencies"`
		PeerDeps   map[string]any  `json:"peerDependencies"`
		OptDeps    map[string]any  `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(src, &pkg); err != nil {
		return err
	}
	x.f.PkgName = pkg.Name
	var deps []string
	for _, m := range []map[string]any{pkg.Deps, pkg.DevDeps, pkg.PeerDeps, pkg.OptDeps} {
		for name := range m {
			deps = append(deps, name)
		}
	}
	slices.Sort(deps)
	for _, d := range slices.Compact(deps) {
		x.add(facts.ImportDepend, d, "")
	}
	var globs []string
	if json.Unmarshal(pkg.Workspaces, &globs) != nil {
		var ws struct {
			Packages []string `json:"packages"`
		}
		_ = json.Unmarshal(pkg.Workspaces, &ws)
		globs = ws.Packages
	}
	for _, g := range globs {
		if p := x.rel(g); p != "" {
			x.add(facts.ImportRoot, "", p)
		}
	}
	x.npmExports(pkg.Exports)
	for _, entry := range []string{pkg.Source, pkg.Types, pkg.Typings, pkg.Module, pkg.Main} {
		x.export(".", entry)
	}
	return nil
}

// npmExports records package.json "exports": a string, a map of subpaths,
// or a map of conditions; for conditions, the first of types, import,
// default, require that names a file.
func (x *extractor) npmExports(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		x.export(".", s)
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	subpaths := false
	for k := range m {
		subpaths = subpaths || strings.HasPrefix(k, ".")
	}
	if !subpaths {
		x.export(".", condition(raw))
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		x.export(k, condition(m[k]))
	}
}

func condition(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, c := range []string{"types", "import", "default", "require", "node"} {
		if v, ok := m[c]; ok {
			if t := condition(v); t != "" {
				return t
			}
		}
	}
	return ""
}

func (x *extractor) export(subpath, target string) {
	p := x.rel(target)
	if p == "" {
		return
	}
	e := facts.Export{Name: subpath, Source: p, Line: 1}
	if !slices.Contains(x.f.Exports, e) {
		x.f.Exports = append(x.f.Exports, e)
	}
}

func (x *extractor) pnpm(src []byte) error {
	var ws struct {
		Packages []string `yaml:"packages"`
	}
	if err := yaml.Unmarshal(src, &ws); err != nil {
		return err
	}
	for _, g := range ws.Packages {
		if strings.HasPrefix(g, "!") {
			continue
		}
		if p := x.rel(g); p != "" {
			x.add(facts.ImportRoot, "", p)
		}
	}
	return nil
}

func (x *extractor) tsconfig(src []byte) error {
	var cfg struct {
		Extends         json.RawMessage `json:"extends"`
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(jsonc(src), &cfg); err != nil {
		return err
	}
	var extends []string
	if json.Unmarshal(cfg.Extends, &extends) != nil {
		var one string
		if json.Unmarshal(cfg.Extends, &one) == nil {
			extends = []string{one}
		}
	}
	for _, e := range extends {
		if strings.HasPrefix(e, ".") {
			if !strings.HasSuffix(e, ".json") {
				e += ".json"
			}
			if p := x.rel(e); p != "" {
				x.add(facts.ImportModule, "", p)
			}
		}
	}
	base := x.dir
	if cfg.CompilerOptions.BaseURL != "" {
		base = x.rel(cfg.CompilerOptions.BaseURL)
		if base == "" {
			return nil
		}
		x.add(facts.ImportRoot, "", base)
	}
	patterns := make([]string, 0, len(cfg.CompilerOptions.Paths))
	for k := range cfg.CompilerOptions.Paths {
		patterns = append(patterns, k)
	}
	slices.Sort(patterns)
	for _, pat := range patterns {
		for _, target := range cfg.CompilerOptions.Paths[pat] {
			t := strings.ReplaceAll(strings.TrimSpace(target), "\\", "/")
			if t == "" || path.IsAbs(t) {
				continue
			}
			p := path.Join(base, t)
			if strings.HasSuffix(t, "/*") || t == "*" {
				p = strings.TrimSuffix(path.Join(base, strings.TrimSuffix(t, "*")), "/") + "/*"
				if t == "*" {
					p = strings.TrimPrefix(path.Join(base, ".")+"/*", "./")
				}
			}
			if p == ".." || strings.HasPrefix(p, "../") {
				continue
			}
			x.add(facts.ImportAlias, pat, p)
		}
	}
	return nil
}

// jsonc strips comments and trailing commas from JSON with comments.
func jsonc(src []byte) []byte {
	var out bytes.Buffer
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inStr:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return out.Bytes()
			}
			i += end + 3
		case c == ',':
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

// --- Rust and Python (TOML) ---

func (x *extractor) cargo(src []byte) {
	t := parseTOML(src)
	x.f.PkgName = unquoteTOML(t.get("package", "name"))
	if lib := unquoteTOML(t.get("lib", "name")); lib != "" {
		x.f.PkgName = lib
	}
	for _, m := range tomlStrings(t.get("workspace", "members")) {
		if p := x.rel(m); p != "" {
			x.add(facts.ImportRoot, "", p)
		}
	}
	for _, sec := range t.order {
		switch {
		case isDepTable(sec):
			for _, k := range t.keys[sec] {
				x.cargoDep(k, inlineTable(t.vals[sec][k]))
			}
		case isDepTable(sec[:max(strings.LastIndexByte(sec, '.'), 0)]):
			// [dependencies.name] with the fields as keys
			x.cargoDep(sec[strings.LastIndexByte(sec, '.')+1:], t.vals[sec])
		}
	}
}

func isDepTable(sec string) bool {
	switch sec {
	case "dependencies", "dev-dependencies", "build-dependencies", "workspace.dependencies":
		return true
	}
	return strings.HasPrefix(sec, "target.") &&
		(strings.HasSuffix(sec, ".dependencies") || strings.HasSuffix(sec, ".dev-dependencies"))
}

// cargoDep records a dependency under the name code uses for it (the key,
// with "-" as "_"), with its path if it has one.
func (x *extractor) cargoDep(key string, fields map[string]string) {
	p := ""
	if raw := fields["path"]; raw != "" {
		p = x.rel(unquoteTOML(raw))
	}
	x.add(facts.ImportDepend, strings.ReplaceAll(key, "-", "_"), p)
}

func (x *extractor) pyproject(src []byte) {
	t := parseTOML(src)
	x.f.PkgName = unquoteTOML(t.get("project", "name"))
	if x.f.PkgName == "" {
		x.f.PkgName = unquoteTOML(t.get("tool.poetry", "name"))
	}
	var roots []string
	roots = append(roots, tomlStrings(t.get("tool.setuptools.packages.find", "where"))...)
	if pd := inlineTable(t.get("tool.setuptools", "package-dir")); pd[`""`] != "" {
		roots = append(roots, unquoteTOML(pd[`""`]))
	}
	for _, m := range tomlTables(t.get("tool.poetry", "packages")) {
		if from := unquoteTOML(m["from"]); from != "" {
			roots = append(roots, from)
		}
	}
	for _, r := range roots {
		if p := x.rel(r); p != "" {
			x.add(facts.ImportRoot, "", p)
		}
	}
}

// --- PHP ---

func (x *extractor) composer(src []byte) error {
	var c struct {
		Name     string `json:"name"`
		Autoload struct {
			PSR4 map[string]json.RawMessage `json:"psr-4"`
			PSR0 map[string]json.RawMessage `json:"psr-0"`
		} `json:"autoload"`
		AutoloadDev struct {
			PSR4 map[string]json.RawMessage `json:"psr-4"`
		} `json:"autoload-dev"`
	}
	if err := json.Unmarshal(src, &c); err != nil {
		return err
	}
	x.f.PkgName = c.Name
	for _, m := range []map[string]json.RawMessage{c.Autoload.PSR4, c.AutoloadDev.PSR4, c.Autoload.PSR0} {
		prefixes := make([]string, 0, len(m))
		for k := range m {
			prefixes = append(prefixes, k)
		}
		slices.Sort(prefixes)
		for _, prefix := range prefixes {
			var dirs []string
			if json.Unmarshal(m[prefix], &dirs) != nil {
				var one string
				_ = json.Unmarshal(m[prefix], &one)
				dirs = []string{one}
			}
			for _, d := range dirs {
				p := x.rel(d)
				if p == "" && strings.Trim(d, "./") == "" {
					p = x.dir
				}
				if p != "" {
					x.add(facts.ImportAlias, prefix, p)
				}
			}
		}
	}
	return nil
}

// --- JVM ---

func (x *extractor) maven(src []byte) error {
	var pom struct {
		ArtifactID string   `xml:"artifactId"`
		Modules    []string `xml:"modules>module"`
		Deps       []struct {
			ArtifactID string `xml:"artifactId"`
		} `xml:"dependencies>dependency"`
		Build struct {
			Source string `xml:"sourceDirectory"`
			Test   string `xml:"testSourceDirectory"`
		} `xml:"build"`
	}
	if err := xml.Unmarshal(src, &pom); err != nil {
		return err
	}
	x.f.PkgName = pom.ArtifactID
	for _, m := range pom.Modules {
		if p := x.rel(m); p != "" {
			x.add(facts.ImportRoot, "", p)
		}
	}
	for _, d := range pom.Deps {
		if d.ArtifactID != "" {
			x.add(facts.ImportDepend, d.ArtifactID, "")
		}
	}
	return nil
}

var (
	gradleProject = regexp.MustCompile(`project\s*\(\s*(?:path\s*[:=]\s*)?["'](:[^"']*)["']`)
	gradleInclude = regexp.MustCompile(`(?m)^\s*include(?:Build)?\s*\(?((?:\s*["'][^"']+["']\s*,?)+)`)
	gradleName    = regexp.MustCompile(`rootProject\.name\s*=\s*["']([^"']+)["']`)
	quoted        = regexp.MustCompile(`["']([^"']+)["']`)
)

// gradle records project(':a:b') dependencies of a build script as
// ImportDepend with Name ":a:b", and the projects a settings script
// includes as ImportRoot with Name ":a:b" and their default directory.
func (x *extractor) gradle(src []byte) {
	if strings.HasPrefix(path.Base(x.f.Path), "settings.gradle") {
		if m := gradleName.FindSubmatch(src); m != nil {
			x.f.PkgName = string(m[1])
		}
		for _, m := range gradleInclude.FindAllSubmatch(src, -1) {
			for _, q := range quoted.FindAllSubmatch(m[1], -1) {
				proj := string(q[1])
				if !strings.HasPrefix(proj, ":") {
					proj = ":" + proj
				}
				if p := x.rel(strings.ReplaceAll(strings.TrimPrefix(proj, ":"), ":", "/")); p != "" {
					x.add(facts.ImportRoot, proj, p)
				}
			}
		}
		return
	}
	for _, m := range gradleProject.FindAllSubmatch(src, -1) {
		x.add(facts.ImportDepend, string(m[1]), "")
	}
}

func (x *extractor) csproj(src []byte) error {
	var p struct {
		Props []struct {
			AssemblyName  string `xml:"AssemblyName"`
			RootNamespace string `xml:"RootNamespace"`
		} `xml:"PropertyGroup"`
		Items []struct {
			Refs []struct {
				Include string `xml:"Include,attr"`
			} `xml:"ProjectReference"`
		} `xml:"ItemGroup"`
	}
	if err := xml.Unmarshal(src, &p); err != nil {
		return err
	}
	x.f.PkgName = strings.TrimSuffix(path.Base(x.f.Path), ".csproj")
	for _, g := range p.Props {
		if g.AssemblyName != "" {
			x.f.PkgName = g.AssemblyName
		}
	}
	for _, g := range p.Items {
		for _, r := range g.Refs {
			ref := x.rel(r.Include)
			if ref == "" {
				continue
			}
			x.add(facts.ImportDepend, strings.TrimSuffix(path.Base(ref), ".csproj"), path.Dir(ref))
		}
	}
	return nil
}

// --- Bazel and Buck ---

var (
	bazelDeps  = regexp.MustCompile(`(?s)\b(?:deps|exports|runtime_deps|exported_deps|implementation_deps)\s*=\s*\[(.*?)\]`)
	bazelLabel = regexp.MustCompile(`["']([^"']+)["']`)
)

// bazel records every dependency label of a BUILD file's rules as the
// package directory it names: //a/b:c and //a/b are "a/b", :c is the
// file's own package; labels of other repositories are skipped.
func (x *extractor) bazel(src []byte) {
	for _, m := range bazelDeps.FindAllSubmatch(src, -1) {
		for _, l := range bazelLabel.FindAllSubmatch(m[1], -1) {
			label := string(l[1])
			var pkg string
			switch {
			case strings.HasPrefix(label, "//"):
				pkg = strings.TrimPrefix(label, "//")
				if i := strings.IndexByte(pkg, ':'); i >= 0 {
					pkg = pkg[:i]
				}
				if pkg == "" {
					pkg = "."
				}
			case strings.HasPrefix(label, ":"):
				continue // the file's own package
			default:
				continue // another repository or cell (@x//a, cell//a)
			}
			x.add(facts.ImportDepend, "", pkg)
		}
	}
}

// --- C and C++ ---

func (x *extractor) compdb(src []byte) error {
	var cmds []struct {
		Directory string   `json:"directory"`
		Command   string   `json:"command"`
		Arguments []string `json:"arguments"`
	}
	if err := json.Unmarshal(src, &cmds); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range cmds {
		args := c.Arguments
		if len(args) == 0 {
			args = strings.Fields(c.Command)
		}
		for i := 0; i < len(args); i++ {
			a, dir := args[i], ""
			for _, flag := range []string{"-I", "-isystem", "-iquote", "/I"} {
				if a == flag && i+1 < len(args) {
					dir = args[i+1]
					i++
					break
				}
				if strings.HasPrefix(a, flag) && len(a) > len(flag) {
					dir = a[len(flag):]
					break
				}
			}
			if dir == "" {
				continue
			}
			dir = strings.Trim(dir, `"`)
			if !path.IsAbs(dir) && !strings.HasPrefix(dir, "/") {
				dir = path.Join(c.Directory, dir)
			}
			p := x.fromRoot(dir)
			if p != "" && !seen[p] {
				seen[p] = true
				x.add(facts.ImportRoot, "", p)
			}
		}
	}
	return nil
}

// fromRoot turns an absolute path inside the repository into a
// root-relative one; "" outside it.
func (x *extractor) fromRoot(abs string) string {
	root := strings.TrimSuffix(strings.ReplaceAll(x.root, "\\", "/"), "/")
	abs = path.Clean(strings.ReplaceAll(abs, "\\", "/"))
	switch {
	case root == "":
		return ""
	case abs == root:
		return "."
	case strings.HasPrefix(abs, root+"/"):
		return abs[len(root)+1:]
	}
	return ""
}

// --- Dart and Swift ---

func (x *extractor) pubspec(src []byte) error {
	var p struct {
		Name    string               `yaml:"name"`
		Deps    map[string]yaml.Node `yaml:"dependencies"`
		DevDeps map[string]yaml.Node `yaml:"dev_dependencies"`
	}
	if err := yaml.Unmarshal(src, &p); err != nil {
		return err
	}
	x.f.PkgName = p.Name
	for _, m := range []map[string]yaml.Node{p.Deps, p.DevDeps} {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		slices.Sort(names)
		for _, name := range names {
			node := m[name]
			var spec struct {
				Path string `yaml:"path"`
			}
			_ = node.Decode(&spec)
			x.add(facts.ImportDepend, name, x.rel(spec.Path))
		}
	}
	return nil
}

var swiftTarget = regexp.MustCompile(`\.(target|executableTarget|testTarget|macro)\s*\(`)

// swiftpm records each target of a Package.swift as ImportRoot with the
// target's name and directory (path:, else Sources/<name> or
// Tests/<name>), and the package name.
func (x *extractor) swiftpm(src []byte) {
	s := string(src)
	if m := regexp.MustCompile(`Package\s*\(\s*name:\s*"([^"]+)"`).FindStringSubmatch(s); m != nil {
		x.f.PkgName = m[1]
	}
	for _, loc := range swiftTarget.FindAllStringSubmatchIndex(s, -1) {
		body := balanced(s[loc[1]-1:])
		name := regexp.MustCompile(`^\s*\(\s*name:\s*"([^"]+)"`).FindStringSubmatch(body)
		if name == nil {
			continue
		}
		dir := "Sources/" + name[1]
		if s[loc[2]:loc[3]] == "testTarget" {
			dir = "Tests/" + name[1]
		}
		if m := regexp.MustCompile(`\bpath:\s*"([^"]+)"`).FindStringSubmatch(body); m != nil {
			dir = m[1]
		}
		if p := x.rel(dir); p != "" {
			x.add(facts.ImportRoot, name[1], p)
		}
	}
}

// balanced returns s up to the parenthesis closing the one it starts with.
func balanced(s string) string {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return s[:i+1]
			}
		case '"':
			if j := strings.IndexByte(s[i+1:], '"'); j >= 0 {
				i += j + 1
			}
		}
	}
	return s
}
