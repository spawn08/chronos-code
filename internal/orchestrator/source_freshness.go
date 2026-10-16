package orchestrator

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/fsignore"
)

// refreshSourceChanges observes real source/config inputs at tool and assessment
// boundaries. Background generators can change them after a passing test.
func (r *taskRuntime) refreshSourceChanges(enable bool) error {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	if r.sources == nil && !enable {
		return nil
	}
	current := map[string]string{}
	ignore := fsignore.New(r.workspaceRoot)
	bytes := int64(0)
	entries := 0
	err := filepath.WalkDir(r.workspaceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 100000 {
			return fmt.Errorf("source freshness scan exceeds 100000 entries")
		}
		if d.IsDir() {
			if path != r.workspaceRoot && (ignore.Ignored(path, true) || d.Name() == "dist" || d.Name() == "out" || d.Name() == "build" || d.Name() == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || ignore.Ignored(path, false) || !sourceInput(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		if bytes > 64<<20 {
			return fmt.Errorf("source freshness scan exceeds 64 MiB")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.workspaceRoot, path)
		if err != nil {
			return err
		}
		current[filepath.ToSlash(rel)] = fmt.Sprintf("%x", sha256.Sum256(data))
		return nil
	})
	if err != nil {
		return fmt.Errorf("observe source freshness: %w", err)
	}
	if r.sources != nil {
		changed := map[string]bool{}
		for path, hash := range current {
			if r.sources[path] != hash {
				changed[path] = true
			}
		}
		for path := range r.sources {
			if _, exists := current[path]; !exists {
				changed[path] = true
			}
		}
		paths := make([]string, 0, len(changed))
		for path := range changed {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			hash := current[path]
			if hash == "" {
				hash = "deleted"
			}
			if _, err := r.ledger.Record(execution.Event{Type: execution.EventWrite, Paths: []string{path}, Scopes: []execution.Scope{{Kind: execution.ScopeExact, Path: path}}, ContentHash: hash, Detail: "source changed since last observation", Provenance: execution.ProvenanceRuntime, CompletedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
	}
	r.sources = current
	return nil
}

func sourceInput(path string) bool {
	if filepath.Ext(path) == ".json" && strings.Contains(filepath.ToSlash(path), "/data/") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".py", ".rs", ".ts", ".tsx", ".js", ".jsx", ".c", ".h", ".cc", ".cpp", ".java", ".kt", ".cs", ".rb", ".php", ".swift", ".sh", ".sql", ".yaml", ".yml", ".toml", ".json", ".css", ".scss", ".sass", ".less", ".html", ".htm", ".vue", ".svelte", ".proto", ".graphql", ".gql":
		return true
	}
	switch filepath.Base(path) {
	case "Makefile", "go.mod", "go.sum", "Dockerfile", "requirements.txt", "Cargo.lock", "yarn.lock", "bun.lock", "bun.lockb", "poetry.lock", "uv.lock", "Pipfile", "Pipfile.lock":
		return true
	}
	return false
}
