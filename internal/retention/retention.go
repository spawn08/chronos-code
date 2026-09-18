// Package retention inventories and prunes persistent Chronos Code data.
package retention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Policy applies three independent limits. Zero disables only that limit; a
// policy with all zero values is disabled. Candidates are the union of records
// older than MaxAge, records beyond the newest MaxCount, and oldest records
// that do not fit within MaxBytes.
type Policy struct {
	MaxAge   time.Duration `json:"max_age,omitempty"`
	MaxCount int           `json:"max_count,omitempty"`
	MaxBytes int64         `json:"max_bytes,omitempty"`
}

func (p Policy) Enabled() bool { return p.MaxAge > 0 || p.MaxCount > 0 || p.MaxBytes > 0 }

// Item is one independently removable retention unit. Size is an estimate for
// database records and the exact lstat size for regular files.
type Item struct {
	Scope     string    `json:"scope"`
	Key       string    `json:"key"`
	UpdatedAt time.Time `json:"updated_at"`
	Bytes     int64     `json:"bytes"`
}

// Adapter inventories and atomically or rename-first removes one resource.
type Adapter interface {
	Scope() string
	Inventory(context.Context) ([]Item, error)
	Delete(context.Context, []Item) error
}

type limitedAdapter interface{ Limitation() string }

type ScopeStatus struct {
	Scope         string `json:"scope"`
	Items         int    `json:"items"`
	Bytes         int64  `json:"bytes"`
	Candidates    int    `json:"candidates"`
	Reclaimable   int64  `json:"reclaimable_bytes"`
	PolicyEnabled bool   `json:"policy_enabled"`
	Unsupported   string `json:"unsupported,omitempty"`
}

type Result struct {
	DryRun         bool          `json:"dry_run"`
	Candidates     []Item        `json:"candidates,omitempty"`
	ReclaimedItems int           `json:"reclaimed_items"`
	ReclaimedBytes int64         `json:"reclaimed_bytes"`
	Statuses       []ScopeStatus `json:"statuses"`
}

// Manager coordinates adapters. BatchSize bounds each adapter's mutation in a
// single Run call; callers can invoke Run repeatedly to drain a backlog.
type Manager struct {
	Adapters  []Adapter
	Policies  map[string]Policy
	BatchSize int
	Now       func() time.Time
}

func (m *Manager) Status(ctx context.Context, scopes ...string) (Result, error) {
	return m.execute(ctx, true, false, scopes)
}

func (m *Manager) Run(ctx context.Context, dryRun bool, scopes ...string) (Result, error) {
	return m.execute(ctx, dryRun, true, scopes)
}

func (m *Manager) Prune(ctx context.Context, scope string, dryRun bool) (Result, error) {
	if scope == "" {
		return Result{}, fmt.Errorf("retention scope is required")
	}
	return m.Run(ctx, dryRun, scope)
}

func (m *Manager) execute(ctx context.Context, dryRun, mutate bool, scopes []string) (Result, error) {
	type plannedDeletion struct {
		adapter Adapter
		items   []Item
	}
	wanted := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		wanted[scope] = true
	}
	result := Result{DryRun: dryRun}
	seen := make(map[string]bool)
	var planned []plannedDeletion
	batch := m.BatchSize
	if batch <= 0 {
		batch = 100
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	for _, adapter := range m.Adapters {
		scope := adapter.Scope()
		if len(wanted) > 0 && !wanted[scope] {
			continue
		}
		seen[scope] = true
		items, err := adapter.Inventory(ctx)
		if err != nil {
			return result, fmt.Errorf("inventory %s: %w", scope, err)
		}
		sort.Slice(items, func(i, j int) bool {
			if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].UpdatedAt.After(items[j].UpdatedAt)
			}
			return items[i].Key < items[j].Key
		})
		policy := m.Policies[scope]
		if policy.MaxAge < 0 || policy.MaxCount < 0 || policy.MaxBytes < 0 {
			return result, fmt.Errorf("retention policy %s limits must be non-negative", scope)
		}
		selected := selectCandidates(items, policy, now)
		status := ScopeStatus{Scope: scope, Items: len(items), Candidates: len(selected), PolicyEnabled: policy.Enabled()}
		if limited, ok := adapter.(limitedAdapter); ok {
			status.Unsupported = limited.Limitation()
		}
		for _, item := range items {
			status.Bytes += item.Bytes
		}
		for _, item := range selected {
			status.Reclaimable += item.Bytes
		}
		result.Statuses = append(result.Statuses, status)
		if !mutate || len(selected) == 0 {
			continue
		}
		// Delete oldest first. Sorting before truncation is required for bounded
		// runs to select the same deterministic batch as their dry-run.
		sort.Slice(selected, func(i, j int) bool {
			if !selected[i].UpdatedAt.Equal(selected[j].UpdatedAt) {
				return selected[i].UpdatedAt.Before(selected[j].UpdatedAt)
			}
			return selected[i].Key < selected[j].Key
		})
		if len(selected) > batch {
			selected = selected[:batch]
		}
		result.Candidates = append(result.Candidates, selected...)
		for _, item := range selected {
			result.ReclaimedBytes += item.Bytes
		}
		result.ReclaimedItems += len(selected)
		planned = append(planned, plannedDeletion{adapter: adapter, items: selected})
	}
	if len(wanted) > 0 {
		var missing []string
		for scope := range wanted {
			if !seen[scope] {
				missing = append(missing, scope)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return result, fmt.Errorf("unknown retention scope: %v", missing)
		}
	}
	if mutate && !dryRun {
		// Inventory every scope before mutating any of them. This makes a dry-run
		// and real run over the same starting state report identical candidates,
		// including when deleting a session also cascades child records.
		for _, deletion := range planned {
			if err := deletion.adapter.Delete(ctx, deletion.items); err != nil {
				return result, fmt.Errorf("prune %s: %w", deletion.adapter.Scope(), err)
			}
		}
	}
	return result, nil
}

func selectCandidates(items []Item, policy Policy, now time.Time) []Item {
	if !policy.Enabled() {
		return nil
	}
	selected := make(map[string]Item)
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, item := range items {
			if item.UpdatedAt.Before(cutoff) {
				selected[item.Key] = item
			}
		}
	}
	if policy.MaxCount > 0 && len(items) > policy.MaxCount {
		for _, item := range items[policy.MaxCount:] {
			selected[item.Key] = item
		}
	}
	if policy.MaxBytes > 0 {
		var total int64
		for _, item := range items {
			total += item.Bytes
		}
		for i := len(items) - 1; i >= 0 && total > policy.MaxBytes; i-- {
			selected[items[i].Key] = items[i]
			total -= items[i].Bytes
		}
	}
	result := make([]Item, 0, len(selected))
	for _, item := range selected {
		result = append(result, item)
	}
	return result
}

func joinErrors(errs []error) error { return errors.Join(errs...) }
