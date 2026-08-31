package learning

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Store persists pending Suggestions (PRD P3-003) as one YAML file per
// suggestion under dir/pending/, and applies accepted suggestions into
// dir/agents/ (agent kind) or dir/patterns.yaml (pattern kind) — matching
// the PRD's `.chronos-code/learned/{agents/,patterns.yaml}` layout. dir is
// typically LearningConfig.OutputDir (default ".chronos-code/learned").
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir. It does not create dir eagerly;
// subdirectories are created lazily on first write.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) pendingDir() string          { return filepath.Join(s.dir, "pending") }
func (s *Store) pendingPath(id string) string { return filepath.Join(s.pendingDir(), id+".yaml") }

// Save writes sug to the pending directory (creating it if necessary),
// keyed by sug.ID.
func (s *Store) Save(sug *Suggestion) error {
	data, err := yaml.Marshal(sug)
	if err != nil {
		return fmt.Errorf("learning: marshal suggestion: %w", err)
	}
	if err := writeFileAtomic(s.pendingDir(), s.pendingPath(sug.ID), data); err != nil {
		return fmt.Errorf("learning: save suggestion %q: %w", sug.ID, err)
	}
	return nil
}

// List returns all pending suggestions, sorted by CreatedAt ascending. A
// missing pending directory (nothing suggested yet) is not an error.
func (s *Store) List() ([]*Suggestion, error) {
	entries, err := os.ReadDir(s.pendingDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("learning: list pending: %w", err)
	}
	var out []*Suggestion
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		sug, err := s.loadFile(filepath.Join(s.pendingDir(), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, sug)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Get returns the pending suggestion with the given id.
func (s *Store) Get(id string) (*Suggestion, error) {
	return s.loadFile(s.pendingPath(id))
}

func (s *Store) loadFile(path string) (*Suggestion, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("learning: read %s: %w", path, err)
	}
	var sug Suggestion
	if err := yaml.Unmarshal(data, &sug); err != nil {
		return nil, fmt.Errorf("learning: parse %s: %w", path, err)
	}
	return &sug, nil
}

// Reject permanently discards the pending suggestion with the given id.
func (s *Store) Reject(id string) error {
	if err := os.Remove(s.pendingPath(id)); err != nil {
		return fmt.Errorf("learning: reject %q: %w", id, err)
	}
	return nil
}

// patternsDoc is the on-disk shape of dir/patterns.yaml.
type patternsDoc struct {
	Patterns []Suggestion `yaml:"patterns"`
}

// Accept applies the pending suggestion with the given id: an "agent"
// suggestion is written to dir/agents/<agent_id>.yaml, which
// internal/config.Load picks up as a usable agent on the harness's next
// start (PRD P3-002/003's acceptance criterion — a suggestion the user
// approved actually becomes an agent). A "pattern" suggestion is appended to
// dir/patterns.yaml instead. Either way, the pending file is removed after a
// successful apply.
func (s *Store) Accept(id string) error {
	sug, err := s.Get(id)
	if err != nil {
		return err
	}

	switch sug.Kind {
	case "agent":
		if sug.AgentID == "" {
			return fmt.Errorf("learning: suggestion %q has kind=agent but no agent_id", id)
		}
		agentsDir := filepath.Join(s.dir, "agents")
		path := filepath.Join(agentsDir, sug.AgentID+".yaml")
		if err := writeFileAtomic(agentsDir, path, []byte(sug.YAML)); err != nil {
			return fmt.Errorf("learning: write learned agent %q: %w", sug.AgentID, err)
		}
	case "pattern":
		if err := s.appendPattern(sug); err != nil {
			return err
		}
	default:
		return fmt.Errorf("learning: suggestion %q has unknown kind %q", id, sug.Kind)
	}

	if err := os.Remove(s.pendingPath(id)); err != nil {
		return fmt.Errorf("learning: remove pending %q after accept: %w", id, err)
	}
	return nil
}

func (s *Store) appendPattern(sug *Suggestion) error {
	path := filepath.Join(s.dir, "patterns.yaml")
	var doc patternsDoc
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("learning: parse existing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("learning: read %s: %w", path, err)
	}

	doc.Patterns = append(doc.Patterns, *sug)
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("learning: marshal %s: %w", path, err)
	}
	return writeFileAtomic(s.dir, path, data)
}

// UpdateConfidence adjusts the confidence of a pending suggestion by delta
// (positive for positive feedback, negative for negative). Confidence is
// clamped to [0.0, 1.0]. Returns the updated suggestion.
func (s *Store) UpdateConfidence(id string, delta float64) (*Suggestion, error) {
	sug, err := s.Get(id)
	if err != nil {
		return nil, fmt.Errorf("learning: update confidence for %q: %w", id, err)
	}
	sug.Confidence += delta
	if sug.Confidence > 1.0 {
		sug.Confidence = 1.0
	}
	if sug.Confidence < 0.0 {
		sug.Confidence = 0.0
	}
	if err := s.Save(sug); err != nil {
		return nil, err
	}
	return sug, nil
}

// writeFileAtomic marshals data to a temp file inside dir, then renames it
// over path, so a crash mid-write never leaves a corrupted file in place
// (same pattern as internal/memory.Store.save).
func writeFileAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	success = true
	return nil
}
