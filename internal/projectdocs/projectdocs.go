// Package projectdocs discovers, merges, budget-manages, and hot-reloads
// project-level agent instructions (AGENTS.md, CLAUDE.md, and friends) per
// ROADMAP.md §5.4.
package projectdocs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/engine/model"
)

// TokenBudget is the target ceiling for merged project instructions before
// summarization/truncation kicks in (ROADMAP.md §5.4: "Token budget: 16k").
const TokenBudget = 16000

// candidateFiles are checked, in this priority order, at every directory
// level from the workspace root down to the current directory. AGENTS.md,
// CLAUDE.md, and AGENT.md are the chronos-code/industry convention;
// .cursorrules and .github/copilot-instructions.md are read too so a repo
// that already carries Cursor/Copilot instructions doesn't need to
// duplicate them for chronos-code.
var candidateFiles = []string{
	"AGENTS.md",
	"CLAUDE.md",
	"AGENT.md",
	".cursorrules",
	filepath.Join(".github", "copilot-instructions.md"),
}

// Doc is one discovered instructions file.
type Doc struct {
	Path    string // absolute path
	Dir     string // absolute directory it was found in
	RelPath string // Path relative to the Bundle's root, for display
	Body    string
}

// Bundle is the set of instructions files Load discovered, in root-to-leaf
// order: Docs[0] is the file closest to root, Docs[len-1] the file closest
// to cwd, so a straight concatenation already implements ROADMAP.md §5.4's
// "merge order: root → subdirs (subdirs override)."
type Bundle struct {
	Root string
	Docs []Doc
}

// dirsBetween returns every directory level from root (inclusive) down to
// cwd (inclusive) — cwd must be root or a descendant of it.
func dirsBetween(root, cwd string) ([]string, error) {
	root = filepath.Clean(root)
	cwd = filepath.Clean(cwd)

	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("projectdocs: cwd %q is not root %q or a descendant of it", cwd, root)
	}

	dirs := []string{root}
	if rel != "." {
		dir := root
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			dir = filepath.Join(dir, part)
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// WatchDirs returns the directories a Watcher should watch to catch changes
// to any file Load(root, cwd) could have picked up: every level from root to
// cwd, plus each level's ".github" subdirectory (since
// ".github/copilot-instructions.md" is one level deeper than the
// candidateFiles it shares a parent with).
func WatchDirs(root, cwd string) ([]string, error) {
	dirs, err := dirsBetween(root, cwd)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dirs)*2)
	for _, d := range dirs {
		out = append(out, d)
		if info, statErr := os.Stat(filepath.Join(d, ".github")); statErr == nil && info.IsDir() {
			out = append(out, filepath.Join(d, ".github"))
		}
	}
	return out, nil
}

// Load walks from root (inclusive) down to cwd (inclusive) — cwd must be
// root or a descendant of it — collecting every candidateFiles match
// present at each directory level.
func Load(root, cwd string) (*Bundle, error) {
	root = filepath.Clean(root)
	dirs, err := dirsBetween(root, cwd)
	if err != nil {
		return nil, err
	}

	var docs []Doc
	for _, dir := range dirs {
		for _, name := range candidateFiles {
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			relPath, err := filepath.Rel(root, p)
			if err != nil {
				relPath = p
			}
			docs = append(docs, Doc{Path: p, Dir: dir, RelPath: relPath, Body: string(data)})
		}
	}
	return &Bundle{Root: root, Docs: docs}, nil
}

// Paths returns the absolute file paths every Doc in b came from, for a
// caller to pass to Watch.
func (b *Bundle) Paths() []string {
	paths := make([]string, len(b.Docs))
	for i, d := range b.Docs {
		paths[i] = d.Path
	}
	return paths
}

// Empty reports whether no instructions files were found at all.
func (b *Bundle) Empty() bool { return len(b.Docs) == 0 }

// raw concatenates every doc as a <project_instructions source="..."> block
// per ROADMAP.md §5.4, unbudgeted.
func (b *Bundle) raw() string {
	if len(b.Docs) == 0 {
		return ""
	}
	var buf bytes.Buffer
	for _, d := range b.Docs {
		fmt.Fprintf(&buf, "<project_instructions source=%q>\n%s\n</project_instructions>\n", d.RelPath, strings.TrimSpace(d.Body))
	}
	return strings.TrimSpace(buf.String())
}

// Summarizer condenses text that exceeds TokenBudget. Implementations
// should be backed by a fast/cheap model; this package stays
// provider-agnostic and independently unit-testable by taking it as an
// injected function rather than owning a model.Provider itself.
type Summarizer func(ctx context.Context, text string) (string, error)

// Render returns b's merged docs as one string ready for injection at the
// top of a system prompt. If the raw merge fits within TokenBudget (counted
// against modelID via chronos's own tokenizer), it's returned verbatim.
// Otherwise summarize is invoked once per distinct raw-text content hash and
// the result cached at cachePath (ROADMAP.md §5.4: "cache the summary keyed
// by content hash"); if summarize is nil or errors, the text is
// hard-truncated instead, with a marker noting the cut.
func Render(ctx context.Context, b *Bundle, modelID, cachePath string, summarize Summarizer) (string, error) {
	raw := b.raw()
	if raw == "" {
		return "", nil
	}
	counter := model.NewTokenCounter(modelID)
	if counter.CountString(raw) <= TokenBudget {
		return raw, nil
	}

	hash := contentHash(raw)
	cache := loadSummaryCache(cachePath)
	if cached, ok := cache[hash]; ok {
		return cached, nil
	}

	if summarize != nil {
		if summary, err := summarize(ctx, raw); err == nil && strings.TrimSpace(summary) != "" {
			cache[hash] = summary
			cache.save(cachePath)
			return summary, nil
		}
	}
	return hardTruncate(raw, counter, TokenBudget), nil
}

// hardTruncate binary-searches for the longest byte-prefix of raw (plus a
// trailing marker) that fits within budget tokens, per counter. It's the
// fallback path when no Summarizer is configured or the summarize call
// fails — correctness (never exceeding budget) matters more than an exact
// cut point here.
func hardTruncate(raw string, counter model.TokenCounter, budget int) string {
	const marker = "\n\n[project instructions truncated: exceeded token budget]"
	lo, hi, best := 0, len(raw), ""
	for lo <= hi {
		mid := (lo + hi) / 2
		candidate := raw[:mid] + marker
		if counter.CountString(candidate) <= budget {
			best = candidate
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// summaryCache is a tiny content-hash → summary map persisted as JSON, so a
// summarization LLM call only ever runs once per distinct instructions text.
type summaryCache map[string]string

func loadSummaryCache(path string) summaryCache {
	data, err := os.ReadFile(path)
	if err != nil {
		return summaryCache{}
	}
	var c summaryCache
	if json.Unmarshal(data, &c) != nil {
		return summaryCache{}
	}
	return c
}

func (c summaryCache) save(path string) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, data, 0o600)
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
