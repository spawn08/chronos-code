// Package workspace detects project type, indexes files in a .gitignore-aware
// way, and exposes a short banner plus a T0 tool for injecting workspace
// context into agent system prompts.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/tool"
)

// maxFiles caps the number of indexed file paths kept in Info.Files.
const maxFiles = 20000

// gitLsFilesTimeout bounds how long we wait for `git ls-files` before falling
// back to a manual directory walk.
const gitLsFilesTimeout = 5 * time.Second

// skipDirs are directory names (other than root itself) that are excluded,
// along with their contents, from the fallback filesystem walk.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
}

// Info describes a detected workspace: its root, detected languages, and an
// index of its files.
type Info struct {
	Root      string
	Languages []string
	FileCount int
	// Files is capped at maxFiles entries (truncated silently if the
	// workspace has more). FileCount reflects the true total when the
	// indexing source can report it; otherwise it falls back to len(Files).
	Files []string
}

// Detect inspects root, detecting the project's language(s) from top-level
// marker files and indexing its files (preferring `git ls-files` so
// .gitignore rules are respected, falling back to a filtered directory walk).
// It returns an error only if root itself cannot be statted as a directory;
// zero files or an unknown language are not error conditions.
func Detect(root string) (*Info, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: stat root %q: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("workspace: root %q is not a directory", root)
	}

	info := &Info{
		Root:      root,
		Languages: detectLanguages(root),
	}

	files := gitLsFiles(root)
	if files == nil {
		files, err = walkFiles(root)
		if err != nil {
			return nil, fmt.Errorf("workspace: indexing %q: %w", root, err)
		}
	}

	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	info.Files = files
	info.FileCount = len(files)

	return info, nil
}

// detectLanguages checks for top-level marker files under root, in priority
// order, and returns the languages whose markers are present.
func detectLanguages(root string) []string {
	var langs []string
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	if has("go.mod") {
		langs = append(langs, "Go")
	}
	if has("package.json") {
		langs = append(langs, "Node")
	}
	if has("pyproject.toml") || has("requirements.txt") {
		langs = append(langs, "Python")
	}
	if has("Cargo.toml") {
		langs = append(langs, "Rust")
	}
	if has("pom.xml") || has("build.gradle") || has("build.gradle.kts") {
		langs = append(langs, "Java")
	}
	return langs
}

// gitLsFiles attempts to list tracked files via `git -C root ls-files`. It
// returns nil if git is unusable (not installed, not a repo, non-zero exit,
// or timeout), signaling the caller to fall back to a directory walk.
func gitLsFiles(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), gitLsFilesTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}

	lines := strings.Split(stdout.String(), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files
}

// walkFiles collects file paths (relative to root) via filepath.WalkDir,
// skipping directories in skipDirs and any dotdir other than root itself.
func walkFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Banner returns a short 1-2 line human string summarizing the workspace,
// suitable for injection into an LLM system prompt under a tight token
// budget (see internal/cli/root.go's systemPromptTokenBudget).
func (i *Info) Banner() string {
	var lang string
	switch len(i.Languages) {
	case 0:
		lang = "generic"
	default:
		lang = strings.Join(i.Languages, ", ")
	}
	return fmt.Sprintf("Workspace: %s project at %s (%d files indexed).", lang, i.Root, i.FileCount)
}

// Tool returns a T0 (zero-LLM-cost) tool exposing the workspace's root,
// detected language(s), and total indexed file count. It deliberately does
// not return the full Files list; agents should use file_glob/file_list/graph
// tools for enumeration instead.
func Tool(info *Info) *tool.Definition {
	return &tool.Definition{
		Name:        "workspace_info",
		Description: "Get the workspace root path, detected project language(s), and total indexed file count.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{
				"root":       info.Root,
				"languages":  info.Languages,
				"file_count": info.FileCount,
			}, nil
		},
	}
}
