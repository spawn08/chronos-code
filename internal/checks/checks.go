// Package checks selects repository checks and infers bounded, manifest-aware evidence.
package checks

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spawn08/chronos-code/internal/execution"
	"gopkg.in/yaml.v3"
)

//go:embed registry.yaml
var registryYAML []byte

// Check is a runnable recommendation for affected Paths within a manifest root.
// Paths are not evidence; use Infer on the command and match its actual scopes.
type Check struct {
	Family  string
	Root    string
	Command string
	Class   execution.CommandClass
	Paths   []string
}

// CommandEvidence must be matched by both Family and Root, not scopes alone.
// In particular, an ancestor module's recursive scope does not certify a nested module.
type CommandEvidence struct {
	Family          string
	Root            string
	Scopes          []execution.Scope
	DirectInputDirs []string
	Known           bool
}

type language struct {
	Name       string                 `yaml:"name"`
	Manifests  []string               `yaml:"manifests"`
	Extensions []string               `yaml:"extensions"`
	Family     string                 `yaml:"family"`
	Command    string                 `yaml:"command"`
	Class      execution.CommandClass `yaml:"class"`
}
type script struct {
	Name   string                 `yaml:"name"`
	Family string                 `yaml:"family"`
	Class  execution.CommandClass `yaml:"class"`
}
type manager struct {
	Name      string   `yaml:"name"`
	Lockfiles []string `yaml:"lockfiles"`
	Run       string   `yaml:"run"`
	Separator string   `yaml:"separator"`
}
type flagSet struct {
	Boolean []string `yaml:"boolean"`
	Value   []string `yaml:"value"`
}
type registry struct {
	Limits struct {
		ManifestBytes int64 `yaml:"manifest_bytes"`
		TotalBytes    int64 `yaml:"total_bytes"`
		Operations    int   `yaml:"operations"`
		ChangedPaths  int   `yaml:"changed_paths"`
		PathDepth     int   `yaml:"path_depth"`
		CommandBytes  int   `yaml:"command_bytes"`
	} `yaml:"limits"`
	Languages           []language          `yaml:"languages"`
	Scripts             []script            `yaml:"scripts"`
	Managers            []manager           `yaml:"managers"`
	Runners             map[string][]string `yaml:"runners"`
	JSTestRunners       []string            `yaml:"js_test_runners"`
	TestOnce            map[string]string   `yaml:"test_once"`
	JSDiagnosticRunners []string            `yaml:"js_diagnostic_runners"`
	JSBuildRunners      []string            `yaml:"js_build_runners"`
	Flags               map[string]flagSet  `yaml:"flags"`
}
type repository struct {
	ctx             context.Context
	root            string
	lexicalRoot     string
	fs              *os.Root
	reg             registry
	operations      int
	directInputDirs []string
	bytes           int64
}

func openRepository(ctx context.Context, workspace string) (*repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("checks context: %w", err)
	}
	r := &repository{ctx: ctx}
	if err := yaml.Unmarshal(registryYAML, &r.reg); err != nil {
		return nil, fmt.Errorf("decode checks registry: %w", err)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve checks workspace: %w", err)
	}
	r.lexicalRoot = root
	r.root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve checks workspace symlinks: %w", err)
	}
	info, err := os.Stat(r.root)
	if err != nil {
		return nil, fmt.Errorf("inspect checks workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("checks workspace is not a directory: %s", workspace)
	}
	r.fs, err = os.OpenRoot(r.root)
	if err != nil {
		return nil, fmt.Errorf("open checks workspace: %w", err)
	}
	return r, nil
}
func (r *repository) tick() error {
	if err := r.ctx.Err(); err != nil {
		return fmt.Errorf("checks context: %w", err)
	}
	r.operations++
	if r.operations > r.reg.Limits.Operations {
		return fmt.Errorf("checks operation limit exceeded")
	}
	return nil
}
func (r *repository) path(base, value string) (string, error) {
	if err := r.tick(); err != nil {
		return "", err
	}
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("invalid checks path")
	}
	p := value
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, filepath.FromSlash(base), value)
	} else {
		// Preserve the workspace's lexical spelling without bypassing symlink checks.
		lexicalRel, err := filepath.Rel(r.lexicalRoot, filepath.Clean(p))
		if err == nil && lexicalRel != ".." && !strings.HasPrefix(lexicalRel, ".."+string(filepath.Separator)) && !filepath.IsAbs(lexicalRel) {
			p = filepath.Join(r.root, lexicalRel)
		}
	}
	rel, err := filepath.Rel(r.root, filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("relativize checks path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("checks path escapes workspace: %s", value)
	}
	if len(strings.Split(filepath.ToSlash(rel), "/")) > r.reg.Limits.PathDepth {
		return "", fmt.Errorf("checks path depth limit exceeded")
	}
	scope, err := execution.NormalizeScope(r.root, execution.Scope{Kind: execution.ScopeExact, Path: rel})
	if err != nil {
		return "", fmt.Errorf("normalize checks path %q: %w", value, err)
	}
	return scope.Path, nil
}
func (r *repository) stat(path string) (os.FileInfo, error) {
	p, err := r.path(".", path)
	if err != nil {
		return nil, err
	}
	info, err := r.fs.Stat(filepath.FromSlash(p))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect checks path %q: %w", path, err)
	}
	return info, nil
}
func (r *repository) read(path string) ([]byte, error) {
	p, err := r.path(".", path)
	if err != nil {
		return nil, err
	}
	before, err := r.fs.Stat(filepath.FromSlash(p))
	if err != nil {
		return nil, fmt.Errorf("inspect checks manifest %q: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("checks manifest is not a regular file: %s", path)
	}
	if before.Size() > r.reg.Limits.ManifestBytes {
		return nil, fmt.Errorf("checks manifest read limit exceeded: %s", path)
	}
	file, err := r.fs.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, fmt.Errorf("open checks manifest %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect checks manifest %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("checks manifest is not a regular file: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, r.reg.Limits.ManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read checks manifest %q: %w", path, err)
	}
	r.bytes += int64(len(data))
	if int64(len(data)) > r.reg.Limits.ManifestBytes || r.bytes > r.reg.Limits.TotalBytes {
		return nil, fmt.Errorf("checks manifest read limit exceeded: %s", path)
	}
	return data, nil
}
func (r *repository) nearest(dir string, lang language) (string, string, error) {
	for {
		for _, name := range lang.Manifests {
			p := join(dir, name)
			info, err := r.stat(p)
			if err != nil {
				return "", "", err
			}
			if info != nil {
				if _, err := r.read(p); err != nil {
					return "", "", err
				}
				return dir, p, nil
			}
		}
		if dir == "." {
			return "", "", nil
		}
		dir = filepath.ToSlash(filepath.Dir(dir))
	}
}
func join(root, path string) string {
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(root), filepath.FromSlash(path)))
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func quote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\r'\"\\$`;&|<>()*?[]{}!#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
func runnable(root, command string) string {
	if root == "." {
		return command
	}
	return "cd " + quote(root) + " && " + command
}

type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
}

func (r *repository) jsPackage(root string) (packageJSON, error) {
	data, err := r.read(join(root, "package.json"))
	if err != nil {
		return packageJSON{}, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return pkg, fmt.Errorf("decode checks package.json: %w", err)
	}
	return pkg, nil
}
func (r *repository) jsManager(root string, pkg packageJSON) (manager, error) {
	for {
		if pkg.PackageManager != "" {
			name := strings.SplitN(pkg.PackageManager, "@", 2)[0]
			for _, m := range r.reg.Managers {
				if m.Name == name {
					return m, nil
				}
			}
			return manager{}, fmt.Errorf("unsupported package manager %q", pkg.PackageManager)
		}
		for _, m := range r.reg.Managers {
			for _, lock := range m.Lockfiles {
				info, err := r.stat(join(root, lock))
				if err != nil {
					return manager{}, err
				}
				if info != nil {
					return m, nil
				}
			}
		}
		if root == "." {
			break
		}
		root = filepath.ToSlash(filepath.Dir(root))
		pkg = packageJSON{}
		info, err := r.stat(join(root, "package.json"))
		if err != nil {
			return manager{}, err
		}
		if info != nil {
			pkg, err = r.jsPackage(root)
			if err != nil {
				return manager{}, err
			}
		}
	}
	for _, m := range r.reg.Managers {
		if m.Name == "npm" {
			return m, nil
		}
	}
	return manager{}, fmt.Errorf("checks registry has no default package manager")
}

// Select groups affected paths by their nearest relevant manifest. It does not
// execute checks, install tools, or assume a language without a manifest.
func Select(ctx context.Context, workspace string, changed []string) ([]Check, error) {
	r, err := openRepository(ctx, workspace)
	if err != nil {
		return nil, err
	}
	defer r.fs.Close()
	if len(changed) > r.reg.Limits.ChangedPaths {
		return nil, fmt.Errorf("checks changed-path limit exceeded")
	}
	grouped := map[string]*Check{}
	for _, value := range changed {
		path, err := r.path(".", value)
		if err != nil {
			return nil, err
		}
		info, err := r.stat(path)
		if err != nil {
			return nil, err
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if info != nil && info.IsDir() {
			dir = path
		}
		roots, err := r.affectedRoots(dir, path)
		if err != nil {
			return nil, err
		}
		for _, lang := range r.reg.Languages {
			root := roots[lang.Name]
			if root == "" {
				continue
			}
			check := Check{Family: lang.Family, Root: root, Class: lang.Class, Command: lang.Command}
			if lang.Name == "javascript" {
				pkg, err := r.jsPackage(root)
				if err != nil {
					return nil, err
				}
				selected := false
				for _, s := range r.reg.Scripts {
					if strings.TrimSpace(pkg.Scripts[s.Name]) == "" {
						continue
					}
					m, err := r.jsManager(root, pkg)
					if err != nil {
						return nil, err
					}
					check.Family, check.Class, check.Command = s.Family, s.Class, r.scriptCommand(m, s, pkg.Scripts[s.Name])
					selected = true
					break
				}
				if !selected {
					continue
				}
			}
			check.Command = runnable(root, check.Command)
			key := check.Family + "\x00" + root
			if existing := grouped[key]; existing != nil {
				existing.Paths = append(existing.Paths, path)
			} else {
				check.Paths = []string{path}
				grouped[key] = &check
			}
		}
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	checks := make([]Check, 0, len(keys))
	for _, key := range keys {
		c := grouped[key]
		sort.Strings(c.Paths)
		c.Paths = unique(c.Paths)
		checks = append(checks, *c)
	}
	return checks, nil
}
func (r *repository) scriptCommand(m manager, s script, body string) string {
	command := m.Run + " " + quote(s.Name)
	if s.Class != execution.CommandTest {
		return command
	}
	tokens, ok := words(body, r.reg.Limits.CommandBytes)
	if !ok {
		return command
	}
	tokens, ok = normalize(tokens)
	if !ok {
		return command
	}
	once := r.reg.TestOnce[tokens[0]]
	if once == "" || contains(tokens[1:], "run") || contains(tokens[1:], once) {
		return command
	}
	for _, token := range tokens[1:] {
		if strings.HasPrefix(token, "--watch") {
			return command
		}
	}
	return command + m.Separator + " " + once
}

func (r *repository) affectedRoots(dir, path string) (map[string]string, error) {
	selected := map[string]bool{}
	for _, lang := range r.reg.Languages {
		selected[lang.Name] = contains(lang.Manifests, filepath.Base(path)) || contains(lang.Extensions, filepath.Ext(path))
		if lang.Name == "javascript" {
			for _, m := range r.reg.Managers {
				selected[lang.Name] = selected[lang.Name] || contains(m.Lockfiles, filepath.Base(path))
			}
		}
	}
	recognized := false
	for _, yes := range selected {
		recognized = recognized || yes
	}
	roots := map[string]string{}
	nearestDepth := -1
	for _, lang := range r.reg.Languages {
		if recognized && !selected[lang.Name] {
			continue
		}
		root, _, err := r.nearest(dir, lang)
		if err != nil {
			return nil, err
		}
		if root == "" {
			continue
		}
		depth := len(strings.Split(root, "/"))
		if root == "." {
			depth = 0
		}
		if !recognized && depth > nearestDepth {
			roots = map[string]string{}
			nearestDepth = depth
		}
		if recognized || depth == nearestDepth {
			roots[lang.Name] = root
		}
	}
	return roots, nil
}

func unique(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
