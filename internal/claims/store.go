package claims

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Status is the truth-maintenance state of a claim.
type Status string

const (
	// StatusLive means every anchor still matches and every parent is live.
	StatusLive Status = "live"
	// StatusStale means at least one of the claim's own anchors changed.
	StatusStale Status = "stale"
	// StatusDoubted means the claim's anchors match but a claim it was
	// derived from is no longer live.
	StatusDoubted Status = "doubted"
)

// Claim is one anchored belief about the workspace.
type Claim struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Anchors     []Anchor `json:"anchors"`
	DerivedFrom []string `json:"derived_from,omitempty"`
	EvidenceID  string   `json:"evidence_id,omitempty"`
	Status      Status   `json:"status"`
	// Reason explains a non-live status.
	Reason string `json:"reason,omitempty"`
}

// Input describes a new claim. Anchor hashes are computed by the store.
type Input struct {
	Text        string
	Anchors     []Anchor
	DerivedFrom []string
	EvidenceID  string
}

// Change reports a status transition produced by Refresh.
type Change struct {
	ID   string `json:"id"`
	From Status `json:"from"`
	To   Status `json:"to"`
}

type entry struct {
	claim Claim
	// ownReason is non-empty when one of the claim's own anchors is invalid.
	ownReason string
}

// Store holds claims for one workspace. It is safe for concurrent use.
// DerivedFrom may only reference existing claims, so the dependency graph
// is acyclic and creation order is a topological order.
type Store struct {
	root string

	mu      sync.Mutex
	entries map[string]*entry
	order   []string
	next    int
}

// NewStore creates an empty store rooted at the workspace directory root.
func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("claims root %q: %w", root, err)
	}
	return &Store{root: abs, entries: make(map[string]*entry)}, nil
}

// RelPath normalizes path to the store's slash-separated, workspace-relative
// form, rejecting paths outside the workspace.
func (s *Store) RelPath(path string) (string, error) {
	return normalizePath(s.root, path)
}

// Add validates, anchors and stores a new live claim.
func (s *Store) Add(in Input) (Claim, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return Claim{}, fmt.Errorf("claim text is required")
	}
	if len(in.Anchors) == 0 {
		return Claim{}, fmt.Errorf("claim requires at least one anchor")
	}
	anchors := make([]Anchor, 0, len(in.Anchors))
	for _, a := range in.Anchors {
		rel, err := normalizePath(s.root, a.Path)
		if err != nil {
			return Claim{}, err
		}
		a.Path = rel
		lines, ok := fileLines(s.root, rel)
		if !ok {
			return Claim{}, fmt.Errorf("anchor %s: file is not readable", a)
		}
		hash, err := hashAnchor(lines, a)
		if err != nil {
			return Claim{}, err
		}
		a.Hash = hash
		anchors = append(anchors, a)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	parents := make([]string, 0, len(in.DerivedFrom))
	seen := make(map[string]bool, len(in.DerivedFrom))
	for _, id := range in.DerivedFrom {
		if seen[id] {
			continue
		}
		if _, ok := s.entries[id]; !ok {
			return Claim{}, fmt.Errorf("derived_from references unknown claim %q", id)
		}
		seen[id] = true
		parents = append(parents, id)
	}
	s.next++
	c := Claim{
		ID:          fmt.Sprintf("c%d", s.next),
		Text:        text,
		Anchors:     anchors,
		DerivedFrom: parents,
		EvidenceID:  in.EvidenceID,
		Status:      StatusLive,
	}
	e := &entry{claim: c}
	s.entries[c.ID] = e
	s.order = append(s.order, c.ID)
	s.applyDerived(e)
	return cloneClaim(e.claim), nil
}

// Get returns a copy of the claim with id.
func (s *Store) Get(id string) (Claim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return Claim{}, false
	}
	return cloneClaim(e.claim), true
}

// List returns copies of all claims in creation order.
func (s *Store) List() []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Claim, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, cloneClaim(s.entries[id].claim))
	}
	return out
}

// Refresh re-checks anchors on the given workspace paths (all anchors when
// no paths are given), relocates moved spans, then recomputes derived
// status for every claim. A claim whose original content reappears is
// restored. It returns status transitions in creation order.
func (s *Store) Refresh(paths ...string) []Change {
	filter := make(map[string]bool, len(paths))
	for _, p := range paths {
		if rel, err := normalizePath(s.root, p); err == nil {
			filter[rel] = true
		}
	}
	all := len(paths) == 0

	s.mu.Lock()
	defer s.mu.Unlock()
	cache := make(map[string][]string)
	read := func(rel string) []string {
		lines, ok := cache[rel]
		if !ok {
			lines, _ = fileLines(s.root, rel)
			cache[rel] = lines
		}
		return lines
	}

	var changes []Change
	for _, id := range s.order {
		e := s.entries[id]
		before := e.claim.Status
		touched := false
		for _, a := range e.claim.Anchors {
			if all || filter[a.Path] {
				touched = true
				break
			}
		}
		if touched {
			e.ownReason = ""
			for i, a := range e.claim.Anchors {
				found, ok := locate(read(a.Path), a)
				if !ok {
					if e.ownReason == "" {
						e.ownReason = fmt.Sprintf("anchor %s changed", a)
					}
					continue
				}
				e.claim.Anchors[i] = found
			}
		}
		s.applyDerived(e)
		if e.claim.Status != before {
			changes = append(changes, Change{ID: id, From: before, To: e.claim.Status})
		}
	}
	return changes
}

// applyDerived sets e's status from its own anchors and its parents. Parents
// always precede e in creation order, so one ordered pass is a fixed point.
func (s *Store) applyDerived(e *entry) {
	if e.ownReason != "" {
		e.claim.Status, e.claim.Reason = StatusStale, e.ownReason
		return
	}
	for _, pid := range e.claim.DerivedFrom {
		if p := s.entries[pid]; p.claim.Status != StatusLive {
			e.claim.Status = StatusDoubted
			e.claim.Reason = fmt.Sprintf("derived from %s claim %s", p.claim.Status, pid)
			return
		}
	}
	e.claim.Status, e.claim.Reason = StatusLive, ""
}

func cloneClaim(c Claim) Claim {
	c.Anchors = append([]Anchor(nil), c.Anchors...)
	c.DerivedFrom = append([]string(nil), c.DerivedFrom...)
	return c
}
