// Package memory implements a local, git-diffable memory store for
// chronos-code. It is a from-scratch package: it does not wrap or depend on
// chronos's own runtime memory system (github.com/spawn08/chronos/sdk/memory
// is unrelated). Memory records are grouped into exactly three categories —
// project, user, feedback — mirroring how Claude Code's own memory system
// separates concerns, and are persisted as one YAML file per category so
// that changes are easy to diff and review in version control.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Category identifies which memory bucket a Record belongs to.
type Category string

const (
	// CategoryProject holds durable facts about the project (conventions,
	// architecture notes, build quirks, etc).
	CategoryProject Category = "project"
	// CategoryUser holds durable facts/preferences about the user.
	CategoryUser Category = "user"
	// CategoryFeedback holds corrections or standing instructions the user
	// gave during a conversation (e.g. "never do X", "always do Y").
	CategoryFeedback Category = "feedback"
)

// categories is the fixed, ordered list of all valid categories.
var categories = []Category{CategoryProject, CategoryUser, CategoryFeedback}

func isValidCategory(c Category) bool {
	for _, k := range categories {
		if k == c {
			return true
		}
	}
	return false
}

// Record is a single memory entry.
type Record struct {
	ID        string    `yaml:"id"`
	Category  Category  `yaml:"category"`
	Content   string    `yaml:"content"`
	CreatedAt time.Time `yaml:"created_at"`
}

// fileDoc is the on-disk shape of a single category's YAML file.
type fileDoc struct {
	Records []Record `yaml:"records"`
}

// Store is a local, YAML-backed memory store. One file per category is
// stored under dir, e.g. dir/project.yaml, dir/user.yaml, dir/feedback.yaml.
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir. It does not create dir eagerly;
// the directory (and category files) are created lazily on first write.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) pathFor(category Category) string {
	return filepath.Join(s.dir, string(category)+".yaml")
}

// load reads and parses the YAML file for category. A missing file is not
// an error: it is treated as an empty document.
func (s *Store) load(category Category) (fileDoc, error) {
	var doc fileDoc
	data, err := os.ReadFile(s.pathFor(category))
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("memory: read %s: %w", category, err)
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("memory: parse %s: %w", category, err)
	}
	return doc, nil
}

// save writes doc for category atomically: marshal to a temp file in the
// same directory, then rename over the real path.
func (s *Store) save(category Category, doc fileDoc) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("memory: create dir %s: %w", s.dir, err)
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("memory: marshal %s: %w", category, err)
	}

	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("memory: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure we clean up the temp file if anything goes wrong before rename.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.pathFor(category)); err != nil {
		return fmt.Errorf("memory: rename into place for %s: %w", category, err)
	}
	success = true
	return nil
}

func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("memory: generate id: %w", err)
	}
	return "mem_" + hex.EncodeToString(buf), nil
}

// Add validates category, creates a new Record with a generated ID and the
// current time, appends it to the category's YAML file (creating the file
// and store directory if needed), and returns the created Record.
func (s *Store) Add(category Category, content string) (Record, error) {
	if !isValidCategory(category) {
		return Record{}, fmt.Errorf("memory: invalid category %q", category)
	}

	id, err := newID()
	if err != nil {
		return Record{}, err
	}

	rec := Record{
		ID:        id,
		Category:  category,
		Content:   content,
		CreatedAt: time.Now(),
	}

	doc, err := s.load(category)
	if err != nil {
		return Record{}, err
	}
	doc.Records = append(doc.Records, rec)

	if err := s.save(category, doc); err != nil {
		return Record{}, err
	}

	return rec, nil
}

// List returns records for category. If category is the empty string, it
// returns records from all three categories concatenated and sorted by
// CreatedAt ascending. A missing file for a category is treated as empty,
// not an error.
func (s *Store) List(category Category) ([]Record, error) {
	if category != "" {
		if !isValidCategory(category) {
			return nil, fmt.Errorf("memory: invalid category %q", category)
		}
		doc, err := s.load(category)
		if err != nil {
			return nil, err
		}
		return doc.Records, nil
	}

	var all []Record
	for _, c := range categories {
		doc, err := s.load(c)
		if err != nil {
			return nil, err
		}
		all = append(all, doc.Records...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})
	return all, nil
}

// Search returns all records (across all categories) whose Content
// case-insensitively contains query. An empty query matches everything,
// equivalent to List("").
func (s *Store) Search(query string) ([]Record, error) {
	all, err := s.List("")
	if err != nil {
		return nil, err
	}
	if query == "" {
		return all, nil
	}

	needle := strings.ToLower(query)
	var out []Record
	for _, rec := range all {
		if strings.Contains(strings.ToLower(rec.Content), needle) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Forget removes the record with the given id from whichever category file
// contains it, and rewrites that file atomically. It returns a "not found"
// error if no record with that id exists in any category.
func (s *Store) Forget(id string) error {
	for _, c := range categories {
		doc, err := s.load(c)
		if err != nil {
			return err
		}
		for i, rec := range doc.Records {
			if rec.ID == id {
				doc.Records = append(doc.Records[:i], doc.Records[i+1:]...)
				return s.save(c, doc)
			}
		}
	}
	return fmt.Errorf("memory: record %q not found", id)
}

const (
	contextBlockMaxContentLen = 120
	contextBlockMaxTotalLen   = 800
)

// ContextBlock builds a short, human-readable multi-line text summary of the
// most recent maxRecords records (across all categories, most-recent-first).
// It returns "" (with a nil error) if the store has zero records. Each
// record's Content is truncated to roughly 120 characters (with "..."
// appended if truncated), and the whole block is capped at roughly 800
// characters so it stays cheap to inject into a model's context window:
// lines stop being added once that budget would be exceeded.
//
// This is intended to be called once per conversation turn by chronos-code's
// orchestrator, so it must stay fast and fully deterministic (no LLM calls).
func (s *Store) ContextBlock(maxRecords int) (string, error) {
	all, err := s.List("")
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "", nil
	}

	// Most-recent-first.
	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	if maxRecords >= 0 && maxRecords < len(all) {
		all = all[:maxRecords]
	}

	header := "Known project/user/feedback notes:"
	var b strings.Builder
	b.WriteString(header)
	total := len(header)

	for _, rec := range all {
		content := rec.Content
		if len(content) > contextBlockMaxContentLen {
			content = content[:contextBlockMaxContentLen] + "..."
		}
		line := fmt.Sprintf("\n- [%s] %s", rec.Category, content)
		if total+len(line) > contextBlockMaxTotalLen {
			break
		}
		b.WriteString(line)
		total += len(line)
	}

	return b.String(), nil
}

// Extraction trigger phrases. Matching is case-insensitive.
var extractionTriggers = []string{
	"remember",
	"don't ",
	"do not ",
	"never ",
	"always ",
	"from now on",
	"please note",
	"for future reference",
}

// ExtractFromMessage is a deliberately simple, deterministic, zero-LLM-cost
// heuristic that flags user messages likely to contain a standing
// instruction or correction worth remembering. It is NOT a full LLM-based
// extraction pipeline — it's a cheap first pass that a future LLM-based
// distillation engine (PRD P3-002) can later replace or augment.
//
// If userMessage case-insensitively contains any known trigger substring,
// it returns (CategoryFeedback, strings.TrimSpace(userMessage), true).
// Otherwise it returns ("", "", false).
func ExtractFromMessage(userMessage string) (category Category, content string, extracted bool) {
	lower := strings.ToLower(userMessage)
	for _, trigger := range extractionTriggers {
		if strings.Contains(lower, trigger) {
			return CategoryFeedback, strings.TrimSpace(userMessage), true
		}
	}
	return "", "", false
}
