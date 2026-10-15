// Package fsignore decides which workspace paths file search skips: the
// repository's .gitignore rules plus build and dependency directories of every
// common language ecosystem. Searching those directories made recursive grep
// exhaust its scan budget in build output (Rust target/, Next.js .next/) and
// return incomplete or wrong matches.
package fsignore

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ecosystemDirs are directory names skipped even without a .gitignore entry.
// They hold version-control state, dependencies, virtual environments or
// build output, never sources a search is looking for.
var ecosystemDirs = map[string]bool{
	// version control and tool state
	".git": true, ".hg": true, ".svn": true, ".chronos-code": true, ".idea": true, ".vscode-test": true,
	// JavaScript / TypeScript
	"node_modules": true, ".next": true, ".nuxt": true, ".svelte-kit": true, ".turbo": true,
	".parcel-cache": true, ".yarn": true, "bower_components": true,
	// Python
	"__pycache__": true, ".venv": true, "venv": true, ".tox": true, ".nox": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true, ".ipynb_checkpoints": true,
	// Rust, Java/Kotlin/Scala, Go, PHP
	"target": true, ".gradle": true, ".bsp": true, ".metals": true, "vendor": true,
	// C# / .NET
	"obj": true,
	// Swift / Apple
	"Pods": true, "DerivedData": true, ".build": true, ".swiftpm": true,
	// Dart / Flutter, Elixir, Haskell, Zig, C/C++
	".dart_tool": true, "_build": true, "deps": true, ".stack-work": true, "dist-newstyle": true,
	"zig-cache": true, ".zig-cache": true, "zig-out": true, "CMakeFiles": true,
	// generic caches
	".cache": true, "coverage": true, ".nyc_output": true, ".terraform": true,
}

// Matcher reports ignored paths under one root. It is safe for concurrent use.
type Matcher struct {
	root string

	mu    sync.Mutex
	rules map[string][]rule // directory (root-relative, slash) -> its .gitignore rules
}

type rule struct {
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
}

// New returns a matcher for root. Symlinks in root are resolved so paths
// from either form (/tmp and /private/tmp on macOS) compare correctly.
func New(root string) *Matcher {
	return &Matcher{root: canonical(root), rules: make(map[string][]rule)}
}

// canonical resolves symlinks in the deepest existing ancestor of p and
// re-appends the missing tail, so not-yet-existing paths canonicalize too.
func canonical(p string) string {
	p = filepath.Clean(p)
	var tail []string
	for dir := p; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		tail = append(tail, filepath.Base(dir))
	}
}

// DirNames returns the always-skipped directory names, sorted.
func DirNames() []string {
	names := make([]string, 0, len(ecosystemDirs))
	for name := range ecosystemDirs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SkipDirName reports whether a directory with this base name is always
// skipped regardless of ignore files.
func SkipDirName(name string) bool { return ecosystemDirs[name] }

// Ignored reports whether the absolute path p (a directory when isDir) is
// excluded by an ecosystem directory name or a .gitignore rule between the
// root and p. Paths outside the root are never ignored.
func (m *Matcher) Ignored(p string, isDir bool) bool {
	rel, err := filepath.Rel(m.root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		// p may use the unresolved form of the root; resolve its directory.
		rel, err = filepath.Rel(m.root, canonical(p))
	}
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)
	if isDir && ecosystemDirs[path.Base(rel)] {
		return true
	}
	// Later (deeper, later-listed) rules win, as in git.
	ignored := false
	parts := strings.Split(rel, "/")
	for depth := 0; depth < len(parts); depth++ {
		dir := strings.Join(parts[:depth], "/")
		sub := strings.Join(parts[depth:], "/")
		for _, r := range m.dirRules(dir) {
			if r.dirOnly && !isDir {
				continue
			}
			if r.matches(sub) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}

func (m *Matcher) dirRules(dir string) []rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules, ok := m.rules[dir]; ok {
		return rules
	}
	rules := parseIgnoreFile(filepath.Join(m.root, filepath.FromSlash(dir), ".gitignore"))
	m.rules[dir] = rules
	return rules
}

func parseIgnoreFile(name string) []rule {
	f, err := os.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []rule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r rule
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		}
		line = strings.TrimPrefix(line, `\`)
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.anchored, line = true, line[1:]
		} else if strings.Contains(line, "/") {
			r.anchored = true // a slash anywhere but the end anchors to this directory
		}
		if line == "" {
			continue
		}
		r.pattern = line
		rules = append(rules, r)
	}
	return rules
}

// matches tests a rule against a path relative to the rule's directory. An
// unanchored pattern matches any path component sequence's last element; a
// match on an ancestor directory also covers everything below it.
func (r rule) matches(rel string) bool {
	parts := strings.Split(rel, "/")
	if !r.anchored {
		for _, p := range parts {
			if ok, _ := path.Match(r.pattern, p); ok {
				return true
			}
		}
		return false
	}
	// In git, "dir/**" matches everything inside dir but not dir itself, so
	// "!dir/keep" can re-include a file; a trailing ** must match a segment.
	// Matching ancestors below covers the deeper levels, so "dir/*" is
	// "dir/**" without dir itself.
	pattern := r.pattern
	if strings.HasSuffix(pattern, "/**") {
		pattern = strings.TrimSuffix(pattern, "**") + "*"
	}
	for i := 1; i <= len(parts); i++ {
		if globMatch(pattern, strings.Join(parts[:i], "/")) {
			return true
		}
	}
	return false
}

// Match reports whether a slash-separated path matches pattern, where "**"
// matches zero or more whole path segments, "{a,b}" alternates, and other
// segments use path.Match.
func Match(pattern, name string) bool {
	for _, p := range expandBraces(pattern) {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

// expandBraces expands the first "{a,b}" group recursively. Unbalanced braces
// are left literal.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	end := strings.IndexByte(pattern[open:], '}')
	if end < 0 {
		return []string{pattern}
	}
	end += open
	var out []string
	for _, alt := range strings.Split(pattern[open+1:end], ",") {
		out = append(out, expandBraces(pattern[:open]+alt+pattern[end+1:])...)
	}
	return out
}

func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
