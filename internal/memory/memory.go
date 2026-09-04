// Package memory implements a local, git-diffable memory store for
// chronos-code. It is a from-scratch package: it does not wrap or depend on
// chronos's own runtime memory system (github.com/spawn08/chronos/sdk/memory
// is unrelated). Memory records are grouped into exactly three categories —
// project, user, feedback — mirroring how Claude Code's own memory system
// separates concerns, and are persisted as one YAML file per category so
// that changes are easy to diff and review in version control.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/storage"
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
	ID                    string    `yaml:"id"`
	Category              Category  `yaml:"category"`
	Content               string    `yaml:"content"`
	Kind                  string    `yaml:"kind,omitempty"`
	Repository            string    `yaml:"repository,omitempty"`
	Source                string    `yaml:"source,omitempty"`
	Revision              string    `yaml:"revision,omitempty"`
	Fingerprint           string    `yaml:"fingerprint,omitempty"`
	Confidence            float64   `yaml:"confidence,omitempty"`
	Validated             bool      `yaml:"validated,omitempty"`
	Invalidated           bool      `yaml:"invalidated,omitempty"`
	InvalidationCondition string    `yaml:"invalidation_condition,omitempty"`
	CreatedAt             time.Time `yaml:"created_at"`
	UpdatedAt             time.Time `yaml:"updated_at,omitempty"`
}

// fileDoc is the on-disk shape of a single category's YAML file.
type fileDoc struct {
	Version int      `yaml:"version,omitempty"`
	Records []Record `yaml:"records"`
}

// Store is a local, YAML-backed memory store. One file per category is
// stored under dir, e.g. dir/project.yaml, dir/user.yaml, dir/feedback.yaml.
// mu serializes the load-modify-save cycle in Add and Forget so two
// concurrent calls (e.g. two fast turns both triggering auto-extraction)
// can't both load the same snapshot and have one silently overwrite the
// other's write.
type Store struct {
	dir string
	mu  *sync.Mutex
}

// NewStore returns a Store rooted at dir. It does not create dir eagerly;
// the directory (and category files) are created lazily on first write.
func NewStore(dir string) *Store {
	return &Store{dir: dir, mu: &sync.Mutex{}}
}

// ForContext returns the memory partition for ctx's storage tenant. The
// default storage tenant keeps the original local directory layout.
func (s *Store) ForContext(ctx context.Context) *Store {
	tenantID := storage.TenantFromContext(ctx)
	if tenantID == storage.DefaultTenant {
		return s
	}
	sum := sha256.Sum256([]byte(tenantID))
	return &Store{
		dir: filepath.Join(s.dir, "tenants", hex.EncodeToString(sum[:])),
		mu:  s.mu,
	}
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
	for i := range doc.Records {
		doc.Records[i] = migrateRecord(doc.Records[i])
	}
	return doc, nil
}

// migrateRecord makes v1 YAML records readable through the v2 schema without
// altering their ID, category, content, or creation time. Legacy records stay
// unvalidated, so callers cannot inject provenance-free knowledge by default.
func migrateRecord(rec Record) Record {
	if rec.Kind == "" {
		rec.Kind = "fact"
	}
	return rec
}

// save writes doc for category atomically: marshal to a temp file in the
// same directory, then rename over the real path.
func (s *Store) save(category Category, doc fileDoc) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("memory: create dir %s: %w", s.dir, err)
	}

	doc.Version = 2
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

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := newID()
	if err != nil {
		return Record{}, err
	}

	rec := Record{
		ID:                    id,
		Category:              category,
		Content:               content,
		Kind:                  "fact",
		Repository:            filepath.Clean(s.dir),
		Source:                "local",
		Revision:              "current",
		Fingerprint:           "current",
		Confidence:            1,
		Validated:             true,
		InvalidationCondition: "source revision changes",
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
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
	s.mu.Lock()
	defer s.mu.Unlock()

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
	added := false

	for _, rec := range all {
		if !rec.Validated || rec.Invalidated {
			continue
		}
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
		added = true
	}

	if !added {
		return "", nil
	}
	return b.String(), nil
}

// ExtractFromMessage is retained until the TUI-owned legacy call is removed.
// Explicit intent is applied centrally by Orchestrator.Execute, so this helper
// must not write a second copy or persist incidental trigger words.
func ExtractFromMessage(string) (category Category, content string, extracted bool) {
	return "", "", false
}
