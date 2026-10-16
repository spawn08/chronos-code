package checks

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/execution"
)

// words accepts a single literal command, including shell quoting. Expansion,
// operators, redirects, and command substitution are deliberately unsupported.
func words(command string, limit int) ([]string, bool) {
	if len(command) > limit || strings.ContainsAny(command, "\x00\r\n$`;|&<>()") {
		return nil, false
	}
	var out []string
	var word strings.Builder
	var quote rune
	active, escape := false, false
	// Quoted assignment names are arguments, not shell environment prefixes.
	literalName := true
	for _, c := range command {
		if escape {
			if c == '=' && assignmentName(word.String()) {
				return nil, false
			}
			// In double quotes, only shell-special characters lose their backslash.
			if quote == '"' && c != '"' && c != '\\' {
				word.WriteRune('\\')
			}
			word.WriteRune(c)
			active = true
			escape = false
			continue
		}
		if c == '=' && !literalName && assignmentName(word.String()) {
			return nil, false
		}
		if c == '\\' && quote != '\'' {
			if !strings.Contains(word.String(), "=") {
				literalName = false
			}
			escape = true
			active = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			if !strings.Contains(word.String(), "=") {
				literalName = false
			}
			quote = c
			active = true
			continue
		}
		if strings.ContainsRune("#~*?[]{}", c) {
			return nil, false
		}
		if c == ' ' || c == '\t' {
			if active {
				out = append(out, word.String())
				word.Reset()
				active = false
				literalName = true
			}
			continue
		}
		word.WriteRune(c)
		active = true
	}
	if quote != 0 || escape {
		return nil, false
	}
	if active {
		out = append(out, word.String())
	}
	return out, len(out) > 0
}

func assignmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func normalize(tokens []string) ([]string, bool) {
	if len(tokens) > 0 && tokens[0] == "time" {
		tokens = tokens[1:]
		if len(tokens) > 0 && tokens[0] == "-p" {
			tokens = tokens[1:]
		}
	}
	if len(tokens) > 0 && tokens[0] == "env" {
		tokens = tokens[1:]
	}
	for len(tokens) > 0 && strings.Contains(tokens[0], "=") {
		name, _, _ := strings.Cut(tokens[0], "=")
		// Selector-affecting environment (GOFLAGS, PYTEST_ADDOPTS, etc.) is unknown.
		if !contains([]string{"CI", "NO_COLOR", "TERM", "FORCE_COLOR"}, name) {
			return nil, false
		}
		tokens = tokens[1:]
	}
	return tokens, len(tokens) > 0
}
func (r *repository) lang(name string) language {
	for _, l := range r.reg.Languages {
		if l.Name == name {
			return l
		}
	}
	return language{}
}

// Infer recognizes only literal, single verification commands. Unknown selectors
// never produce scopes. cwd is absolute or workspace-relative; empty means '.'.
func Infer(ctx context.Context, workspace, cwd, command string) (CommandEvidence, error) {
	r, err := openRepository(ctx, workspace)
	if err != nil {
		return CommandEvidence{}, err
	}
	defer r.fs.Close()
	if cwd == "" {
		cwd = "."
	}
	dir, err := r.path(".", cwd)
	if err != nil {
		return CommandEvidence{}, err
	}
	info, err := r.stat(dir)
	if err != nil {
		return CommandEvidence{}, err
	}
	if info == nil || !info.IsDir() {
		return CommandEvidence{}, fmt.Errorf("checks cwd is not a directory: %s", cwd)
	}
	tokens, ok := words(command, r.reg.Limits.CommandBytes)
	if !ok {
		return CommandEvidence{}, nil
	}
	tokens, ok = normalize(tokens)
	if !ok {
		return CommandEvidence{}, nil
	}
	switch {
	case contains(r.reg.Runners["go"], tokens[0]):
		if len(tokens) < 2 || !contains([]string{"test", "build", "vet"}, tokens[1]) {
			return CommandEvidence{}, nil
		}
		family := "go-test"
		if tokens[1] == "build" {
			family = "go-build"
		}
		if tokens[1] == "vet" {
			family = "go-diagnostics"
		}
		evidence, err := r.base(dir, "go", family)
		if err != nil {
			return evidence, err
		}
		scopes, known, err := r.goScopes(dir, tokens[2:])
		if known {
			evidence.DirectInputDirs = append([]string(nil), r.directInputDirs...)
		}
		return finish(evidence, scopes, known, err)
	case contains(r.reg.Runners["python"], tokens[0]):
		args := tokens[1:]
		if tokens[0] == "python" || tokens[0] == "python3" {
			if len(args) < 2 || args[0] != "-m" || args[1] != "pytest" {
				return CommandEvidence{}, nil
			}
			args = args[2:]
		}
		evidence, err := r.base(dir, "python", "python-test")
		if err != nil {
			return evidence, err
		}
		scopes, known, err := r.pathScopes(dir, args, r.reg.Flags["python"], true)
		return finish(evidence, scopes, known, err)
	case contains(r.reg.Runners["rust"], tokens[0]):
		if len(tokens) < 2 || !contains([]string{"test", "build", "check"}, tokens[1]) {
			return CommandEvidence{}, nil
		}
		family := "rust-test"
		if tokens[1] == "build" {
			family = "rust-build"
		}
		if tokens[1] == "check" {
			family = "rust-diagnostics"
		}
		return r.rustEvidence(dir, family, tokens[2:])
	case contains(r.reg.Runners["javascript"], tokens[0]):
		return r.jsEvidence(dir, tokens)
	}
	return CommandEvidence{}, nil
}
func finish(e CommandEvidence, scopes []execution.Scope, known bool, err error) (CommandEvidence, error) {
	if err != nil {
		return e, err
	}
	if known && len(scopes) > 0 {
		for i, s := range scopes {
			if s.Kind == execution.ScopeDirectory && s.Path == "." {
				scopes[i] = execution.Scope{Kind: execution.ScopeWorkspace}
			}
		}
		e.Known = true
		e.Scopes = scopes
	}
	return e, nil
}
func (r *repository) base(dir, name, family string) (CommandEvidence, error) {
	root, _, err := r.nearest(dir, r.lang(name))
	if err != nil {
		return CommandEvidence{}, err
	}
	if root == "" {
		root = dir
	}
	return CommandEvidence{Family: family, Root: root}, nil
}
func dynamic(path string) bool { return strings.ContainsAny(path, "*?[]{}~$`\x00") }
func flag(args []string, i int, flags flagSet) (int, bool) {
	name, value, equals := strings.Cut(args[i], "=")
	if contains(flags.Boolean, name) && !equals {
		return i, true
	}
	// Some runners use boolean options with an explicit literal value.
	if contains(flags.Boolean, args[i]) {
		return i, true
	}
	if contains(flags.Value, name) {
		if equals {
			return i, value != "" && !dynamic(value)
		}
		if i+1 < len(args) && args[i+1] != "" && !strings.HasPrefix(args[i+1], "-") && !dynamic(args[i+1]) {
			return i + 1, true
		}
	}
	return i, false
}
func (r *repository) scope(dir, path string) (execution.Scope, bool, error) {
	if dynamic(path) {
		return execution.Scope{}, false, nil
	}
	p, err := r.path(dir, path)
	if err != nil {
		return execution.Scope{}, false, err
	}
	info, err := r.stat(p)
	if err != nil {
		return execution.Scope{}, false, err
	}
	if info == nil {
		return execution.Scope{}, false, nil
	}
	kind := execution.ScopeExact
	if info.IsDir() {
		kind = execution.ScopeDirectory
	} else if !info.Mode().IsRegular() {
		return execution.Scope{}, false, nil
	}
	return execution.Scope{Kind: kind, Path: p}, true, nil
}
func (r *repository) pathScopes(dir string, args []string, flags flagSet, nodes bool) ([]execution.Scope, bool, error) {
	var scopes []execution.Scope
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			next, ok := flag(args, i, flags)
			if !ok {
				return nil, false, nil
			}
			i = next
			continue
		}
		if nodes {
			arg, _, _ = strings.Cut(arg, "::")
			// Parametrized node selectors can contain wildcard-like characters, but
			// the path portion remains literal and the scope is only that test file.
		}
		s, known, err := r.scope(dir, arg)
		if err != nil || !known {
			return nil, false, err
		}
		scopes = append(scopes, s)
	}
	if len(scopes) == 0 {
		s, known, err := r.scope(dir, ".")
		if err != nil || !known {
			return nil, false, err
		}
		scopes = append(scopes, s)
	}
	return dedupScopes(scopes), true, nil
}
func dedupScopes(scopes []execution.Scope) []execution.Scope {
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Path == scopes[j].Path {
			return scopes[i].Kind < scopes[j].Kind
		}
		return scopes[i].Path < scopes[j].Path
	})
	out := scopes[:0]
	for _, s := range scopes {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
func (r *repository) goScopes(dir string, args []string) ([]execution.Scope, bool, error) {
	var scopes []execution.Scope
	var selectors []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			next, ok := flag(args, i, r.reg.Flags["go"])
			if !ok {
				return nil, false, nil
			}
			name, value, equals := strings.Cut(args[i], "=")
			if name == "-count" {
				if !equals {
					value = args[next]
				}
				count, err := strconv.Atoi(value)
				if err != nil || count <= 0 {
					return nil, false, nil
				}
			}
			if name == "-coverprofile" {
				if !equals {
					value = args[next]
				}
				if _, err := r.path(dir, value); err != nil {
					return nil, false, err
				}
			}
			i = next
			continue
		}
		selectors = append(selectors, args[i])
	}
	if len(selectors) == 0 {
		selectors = []string{"."}
	}
	fileMode := strings.HasSuffix(selectors[0], ".go")
	for _, selector := range selectors {
		if strings.HasSuffix(selector, ".go") != fileMode {
			return nil, false, nil
		}
		if fileMode {
			s, known, err := r.scope(dir, selector)
			if err != nil || !known || s.Kind != execution.ScopeExact {
				return nil, false, err
			}
			scopes = append(scopes, s)
			continue
		}
		recursive := selector == "./..." || strings.HasSuffix(selector, "/...")
		path := selector
		if recursive {
			path = strings.TrimSuffix(selector, "/...")
			if path == "" {
				path = "."
			}
		}
		if path != "." && !strings.HasPrefix(path, "./") && !strings.HasPrefix(path, "../") && !filepath.IsAbs(path) {
			return nil, false, nil
		}
		s, known, err := r.scope(dir, path)
		if err != nil || !known || s.Kind != execution.ScopeDirectory {
			return nil, false, err
		}
		if recursive {
			scopes = append(scopes, s)
		} else {
			r.directInputDirs = append(r.directInputDirs, s.Path)
			files, err := r.goFiles(s.Path)
			if err != nil {
				return nil, false, err
			}
			if len(files) == 0 {
				return nil, false, nil
			}
			scopes = append(scopes, files...)
		}
	}
	return dedupScopes(scopes), true, nil
}
func (r *repository) goFiles(dir string) ([]execution.Scope, error) {
	if err := r.tick(); err != nil {
		return nil, err
	}
	f, err := r.fs.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, fmt.Errorf("open Go package: %w", err)
	}
	defer f.Close()
	var scopes []execution.Scope
	for {
		entries, err := f.ReadDir(64)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read Go package: %w", err)
		}
		for _, entry := range entries {
			if err := r.tick(); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(entry.Name(), ".go") || entry.IsDir() {
				continue
			}
			s, known, err := r.scope(dir, entry.Name())
			if err != nil {
				return nil, err
			}
			if known {
				scopes = append(scopes, s)
			}
		}
		if err == io.EOF {
			break
		}
	}
	return scopes, nil
}

func (r *repository) rustEvidence(dir, family string, args []string) (CommandEvidence, error) {
	evidence, err := r.base(dir, "rust", family)
	if err != nil {
		return evidence, err
	}
	manifest, packageName := "", ""
	known := true
	workspace := false
	for i := 0; i < len(args); i++ {
		name, value, equals := strings.Cut(args[i], "=")
		if name == "--manifest-path" || name == "--package" || name == "-p" {
			if !equals {
				if i+1 >= len(args) {
					known = false
					break
				}
				i++
				value = args[i]
			}
			if value == "" || dynamic(value) {
				known = false
				break
			}
			if name == "--manifest-path" {
				if manifest != "" {
					known = false
					break
				}
				manifest = value
			} else {
				if packageName != "" {
					known = false
					break
				}
				packageName = value
			}
			continue
		}
		if name == "--workspace" || name == "--all" {
			workspace = true
		}
		next, ok := flag(args, i, r.reg.Flags["rust"])
		if !ok {
			known = false
			break
		}
		i = next
	}
	if manifest != "" {
		p, err := r.path(dir, manifest)
		if err != nil {
			return evidence, err
		}
		if filepath.Base(p) != "Cargo.toml" {
			return evidence, nil
		}
		if _, err := r.read(p); err != nil {
			return evidence, err
		}
		evidence.Root = filepath.ToSlash(filepath.Dir(p))
	}
	if !known || (workspace && packageName != "") {
		return evidence, nil
	}
	if packageName != "" {
		root, ok, err := r.cargoPackage(evidence.Root, packageName)
		if err != nil || !ok {
			return evidence, err
		}
		evidence.Root = root
	}
	s, ok, err := r.scope(evidence.Root, ".")
	return finish(evidence, []execution.Scope{s}, ok, err)
}

var tomlSection = regexp.MustCompile(`(?m)^\s*\[([^\]]+)\]\s*(?:#.*)?$`)
var cargoName = regexp.MustCompile(`(?m)^\s*name\s*=\s*["']([^"']+)["']\s*(?:#.*)?$`)
var cargoMembers = regexp.MustCompile(`(?s)\bmembers\s*=\s*\[([^\]]*)\]`)
var tomlString = regexp.MustCompile(`["']([^"']+)["']`)

func section(data []byte, name string) string {
	text := string(data)
	matches := tomlSection.FindAllStringSubmatchIndex(text, -1)
	for i, m := range matches {
		if text[m[2]:m[3]] != name {
			continue
		}
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		return text[m[1]:end]
	}
	return ""
}
func (r *repository) cargoPackage(root, name string) (string, bool, error) {
	data, err := r.read(join(root, "Cargo.toml"))
	if err != nil {
		return "", false, err
	}
	match := cargoName.FindStringSubmatch(section(data, "package"))
	if len(match) > 1 && match[1] == name {
		return root, true, nil
	}
	members := cargoMembers.FindStringSubmatch(section(data, "workspace"))
	if len(members) < 2 {
		return "", false, nil
	}
	// Only an explicit literal member list is supported, never globs or discovery.
	values := tomlString.FindAllStringSubmatch(members[1], -1)
	remaining := tomlString.ReplaceAllString(members[1], "")
	if strings.Trim(remaining, " \r\n\t,") != "" {
		return "", false, nil
	}
	found := ""
	for _, value := range values {
		if dynamic(value[1]) {
			return "", false, nil
		}
		dir, err := r.path(root, value[1])
		if err != nil {
			return "", false, err
		}
		data, err := r.read(join(dir, "Cargo.toml"))
		if err != nil {
			return "", false, err
		}
		match := cargoName.FindStringSubmatch(section(data, "package"))
		if len(match) > 1 && match[1] == name {
			if found != "" {
				return "", false, nil
			}
			found = dir
		}
	}
	return found, found != "", nil
}

func (r *repository) jsEvidence(dir string, tokens []string) (CommandEvidence, error) {
	root, manifest, err := r.nearest(dir, r.lang("javascript"))
	if err != nil {
		return CommandEvidence{}, err
	}
	if root == "" {
		root = dir
	}
	evidence := CommandEvidence{Root: root}
	args := tokens[1:]
	if len(args) == 0 {
		return evidence, nil
	}
	if tokens[0] == "bun" && args[0] == "test" {
		evidence.Family = "javascript-test"
		scopes, known, err := r.jsTestScopes(dir, args[1:], r.reg.Flags["js_test"])
		return finish(evidence, scopes, known, err)
	}
	explicit := args[0] == "run" || (tokens[0] == "npm" && args[0] == "run-script")
	if explicit {
		args = args[1:]
	} else {
		if tokens[0] == "bun" || (tokens[0] == "npm" && args[0] != "test") {
			return evidence, nil
		}
	}
	if len(args) == 0 {
		return evidence, nil
	}
	name := args[0]
	args = args[1:]
	var selected script
	for _, s := range r.reg.Scripts {
		if s.Name == name {
			selected = s
			break
		}
	}
	if selected.Name == "" {
		return evidence, nil
	}
	evidence.Family = selected.Family
	if manifest == "" {
		return evidence, nil
	}
	pkg, err := r.jsPackage(root)
	if err != nil {
		return evidence, err
	}
	if pkg.Scripts["pre"+name] != "" || pkg.Scripts["post"+name] != "" {
		return evidence, nil
	}
	scriptTokens, ok := words(pkg.Scripts[name], r.reg.Limits.CommandBytes)
	if !ok {
		return evidence, nil
	}
	scriptTokens, ok = normalize(scriptTokens)
	if !ok {
		return evidence, nil
	}
	runner := scriptTokens[0]
	scriptArgs := scriptTokens[1:]
	flagKind := ""
	switch selected.Class {
	case execution.CommandTest:
		if !contains(r.reg.JSTestRunners, runner) {
			return evidence, nil
		}
		flagKind = "js_test"
		if runner == "bun" {
			if len(scriptArgs) == 0 || scriptArgs[0] != "test" {
				return evidence, nil
			}
			scriptArgs = scriptArgs[1:]
		}
		if runner == "vitest" && len(scriptArgs) > 0 && scriptArgs[0] == "run" {
			scriptArgs = scriptArgs[1:]
		}
		// Interactive/watch commands are not completed verification checks.
		if runner == "vitest" && !contains(scriptTokens[1:], "run") && !contains(scriptTokens[1:], "--run") && !contains(args, "--run") && !contains(args, "run") {
			return evidence, nil
		}
	case execution.CommandDiagnostics:
		if !contains(r.reg.JSDiagnosticRunners, runner) {
			return evidence, nil
		}
		flagKind = "js_diagnostics"
	case execution.CommandBuild:
		if !contains(r.reg.JSBuildRunners, runner) {
			return evidence, nil
		}
		flagKind = "js_build"
		if runner == "vite" || runner == "next" {
			if len(scriptArgs) == 0 || scriptArgs[0] != "build" {
				return evidence, nil
			}
			scriptArgs = scriptArgs[1:]
		}
	default:
		return evidence, nil
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	// Package managers run scripts at the package root, not the invoking subdir.
	combined := append(append([]string{}, scriptArgs...), args...)
	if selected.Class == execution.CommandTest {
		if len(combined) > 0 && combined[0] == "run" {
			combined = combined[1:]
		}
		scopes, known, err := r.jsTestScopes(root, combined, r.reg.Flags[flagKind])
		return finish(evidence, scopes, known, err)
	}
	// Compiler file arguments do not imply coverage of the containing package.
	if runner == "tsc" || runner == "vue-tsc" {
		for i := 0; i < len(combined); i++ {
			next, ok := flag(combined, i, r.reg.Flags[flagKind])
			if !ok {
				return evidence, nil
			}
			i = next
		}
		s, known, err := r.scope(root, ".")
		return finish(evidence, []execution.Scope{s}, known, err)
	}
	if selected.Class == execution.CommandBuild {
		for i := 0; i < len(combined); i++ {
			next, ok := flag(combined, i, r.reg.Flags[flagKind])
			if !ok {
				return evidence, nil
			}
			i = next
		}
		s, known, err := r.scope(root, ".")
		return finish(evidence, []execution.Scope{s}, known, err)
	}
	scopes, known, err := r.pathScopes(root, combined, r.reg.Flags[flagKind], false)
	return finish(evidence, scopes, known, err)
}
func (r *repository) jsTestScopes(root string, args []string, flags flagSet) ([]execution.Scope, bool, error) {
	var scopes []execution.Scope
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--runTestsByPath" {
			if i == len(args)-1 {
				return nil, false, nil
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			next, ok := flag(args, i, flags)
			if !ok {
				return nil, false, nil
			}
			i = next
			continue
		}
		// Jest/Vitest positional selectors are patterns. Only an existing literal
		// file or directory can be translated into evidence; regex filters cannot.
		if strings.ContainsAny(arg, "^+()|") {
			return nil, false, nil
		}
		s, known, err := r.scope(root, arg)
		if err != nil || !known {
			return nil, false, err
		}
		scopes = append(scopes, s)
	}
	if len(scopes) == 0 {
		s, known, err := r.scope(root, ".")
		return []execution.Scope{s}, known, err
	}
	return dedupScopes(scopes), true, nil
}
