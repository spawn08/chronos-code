// Package session implements session lifecycle management (create/resume/
// list/delete/export) on top of chronos's storage.Storage interface.
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spawn08/chronos/storage"
)

// NewSessionID returns a new unique session id of the form "sess_" followed
// by 16 random hex characters (8 random bytes). It uses crypto/rand, so
// collisions are not a practical concern.
func NewSessionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read failing is effectively unrecoverable on any
		// supported platform; fall back to a timestamp-derived id rather
		// than panicking.
		return fmt.Sprintf("sess_%016x", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(buf)
}

// Manager implements session lifecycle operations (create/resume/list/
// delete/export) on top of a storage.Storage backend.
type Manager struct {
	store storage.Storage
}

// NewManager creates a Manager backed by store. The dsn parameter is retained
// for source compatibility; session deletion is performed by the storage
// backend so it remains tenant-scoped.
func NewManager(store storage.Storage, _ string) *Manager {
	return &Manager{store: store}
}

// Ensure guarantees a session with the given sessionID exists and belongs to
// agentID. If no such session exists, one is created with Status "running".
// If a session with sessionID already exists but belongs to a different
// agent, an error is returned. A transient/genuine error from GetSession
// (anything other than "no such row") is propagated rather than treated as
// "session doesn't exist," so it can't be masked by a confusing
// CreateSession failure (e.g. a UNIQUE-constraint error on a row that
// actually exists under a different agent).
func (m *Manager) Ensure(ctx context.Context, sessionID, agentID string) error {
	existing, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get session %q: %w", sessionID, err)
		}
		now := time.Now()
		return m.store.CreateSession(ctx, &storage.Session{
			ID:        sessionID,
			AgentID:   agentID,
			Status:    "running",
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if existing.AgentID != agentID {
		return fmt.Errorf("session %q belongs to agent %q, not %q", sessionID, existing.AgentID, agentID)
	}
	return nil
}

// List returns sessions for agentID, thinly passing through to the
// underlying storage.
func (m *Manager) List(ctx context.Context, agentID string, limit, offset int) ([]*storage.Session, error) {
	return m.store.ListSessions(ctx, agentID, limit, offset)
}

// Latest returns the most recent session for agentID, or (nil, nil) if the
// agent has no sessions. Absence of a session is not treated as an error.
func (m *Manager) Latest(ctx context.Context, agentID string) (*storage.Session, error) {
	sessions, err := m.List(ctx, agentID, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	return sessions[0], nil
}

// Touch loads the session, optionally updates its status, and persists an
// updated UpdatedAt timestamp.
func (m *Manager) Touch(ctx context.Context, sessionID string, status string) error {
	sess, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session %q: %w", sessionID, err)
	}
	if status != "" {
		sess.Status = status
	}
	sess.UpdatedAt = time.Now()
	return m.store.UpdateSession(ctx, sess)
}

// Delete permanently removes a session and all of its associated rows
// (events, checkpoints, traces, audit logs, session-scoped memory, and files).
// The optional SessionDeleter capability keeps deletion in the backend that
// owns tenant isolation. Deleting rows that don't exist is not an error.
func (m *Manager) Delete(ctx context.Context, sessionID string) error {
	deleter, ok := m.store.(storage.SessionDeleter)
	if !ok {
		return fmt.Errorf("storage backend does not support session deletion")
	}
	if err := deleter.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session %q: %w", sessionID, err)
	}
	return nil
}

// ExportedSession is the JSON envelope written by Export, bundling a session
// with all of its ledger events, checkpoints, and traces.
type ExportedSession struct {
	Session     *storage.Session      `json:"session"`
	Events      []*storage.Event      `json:"events"`
	Checkpoints []*storage.Checkpoint `json:"checkpoints"`
	Traces      []*storage.Trace      `json:"traces"`
}

// Export loads sessionID's full state (session record, events, checkpoints,
// traces) via the Storage interface and writes it as indented JSON to path,
// creating any missing parent directories.
func (m *Manager) Export(ctx context.Context, sessionID, path string) error {
	sess, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session %q: %w", sessionID, err)
	}
	events, err := m.store.ListEvents(ctx, sessionID, 0)
	if err != nil {
		return fmt.Errorf("list events for %q: %w", sessionID, err)
	}
	checkpoints, err := m.store.ListCheckpoints(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list checkpoints for %q: %w", sessionID, err)
	}
	traces, err := m.store.ListTraces(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list traces for %q: %w", sessionID, err)
	}

	exported := ExportedSession{
		Session:     sess,
		Events:      events,
		Checkpoints: checkpoints,
		Traces:      traces,
	}
	data, err := json.MarshalIndent(exported, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal exported session: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
