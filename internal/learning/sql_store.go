package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// Session is one agent execution recorded by SQLStore.
type Session struct {
	ID           string
	RepoPath     string
	StartedAt    time.Time
	EndedAt      *time.Time
	Model        string
	Turns        int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// Turn is one message within a Session.
type Turn struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	Timestamp time.Time
}

// ToolCall records one tool invocation associated with a Turn.
type ToolCall struct {
	ID        string
	TurnID    string
	Name      string
	Input     string
	Output    string
	Duration  time.Duration
	Timestamp time.Time
}

// Outcome records the user's disposition of a Turn.
type Outcome struct {
	TurnID    string
	Kind      string
	UserEdit  string
	Timestamp time.Time
}

// StatsReport is a deterministic aggregate of the telemetry database.
type StatsReport struct {
	Sessions  int64
	Turns     int64
	ToolCalls int64
	Outcomes  int64
	Patterns  int64
	Accepted  int64
	Rejected  int64
	Edited    int64
	Reverted  int64
}

// SQLStore is an additive SQLite telemetry store. The existing YAML Store is
// intentionally independent of it.
type SQLStore struct {
	db *sql.DB
}

// OpenSQLStore opens path, applies connection safety settings, and migrates
// the database to the latest supported schema.
func OpenSQLStore(ctx context.Context, path string) (*SQLStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("learning: open SQL store: %w", err)
	}
	// A single pooled connection serializes writers and ensures connection-local
	// SQLite pragmas apply to every operation.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("learning: configure SQL store: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLStore{db: db}, nil
}

// CreateSession records a session if it does not already exist. Session IDs
// survive process restarts, so resuming a conversation must preserve its
// existing telemetry row rather than fail the first hook in the new process.
func (s *SQLStore) CreateSession(ctx context.Context, session Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions
			(id, repo_path, started_at, ended_at, model, turns, input_tokens, output_tokens, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		session.ID, session.RepoPath, timestamp(session.StartedAt), nullableTimestamp(session.EndedAt),
		session.Model, session.Turns, session.InputTokens, session.OutputTokens, session.CostUSD)
	if err != nil {
		return fmt.Errorf("learning: create session %q: %w", session.ID, err)
	}
	return nil
}

// AppendTurn records a new turn in an existing session.
func (s *SQLStore) AppendTurn(ctx context.Context, turn Turn) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO turns (id, session_id, role, content, ts)
		VALUES (?, ?, ?, ?, ?)`, turn.ID, turn.SessionID, turn.Role, turn.Content, timestamp(turn.Timestamp))
	if err != nil {
		return fmt.Errorf("learning: append turn %q: %w", turn.ID, err)
	}
	return nil
}

// RecordToolCall records a tool invocation in an existing turn.
func (s *SQLStore) RecordToolCall(ctx context.Context, call ToolCall) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_calls (id, turn_id, name, input, output, duration_ms, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		call.ID, call.TurnID, call.Name, call.Input, call.Output, call.Duration.Milliseconds(), timestamp(call.Timestamp))
	if err != nil {
		return fmt.Errorf("learning: record tool call %q: %w", call.ID, err)
	}
	return nil
}

// RecordOutcome records the disposition of an existing turn.
func (s *SQLStore) RecordOutcome(ctx context.Context, outcome Outcome) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (turn_id, kind, user_edit, ts)
		VALUES (?, ?, ?, ?)`, outcome.TurnID, outcome.Kind, outcome.UserEdit, timestamp(outcome.Timestamp))
	if err != nil {
		return fmt.Errorf("learning: record outcome for turn %q: %w", outcome.TurnID, err)
	}
	return nil
}

// Segments returns user-initiated session segments in session and turn order.
func (s *SQLStore) Segments(ctx context.Context, repoPath string) ([]SessionSegment, error) {
	type turnRecord struct {
		repoPath       string
		sessionStarted time.Time
		turn           Turn
		outcome        *Outcome
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.repo_path, s.started_at, t.id, t.session_id, t.role, t.content, t.ts,
			o.kind, o.user_edit, o.ts
		FROM sessions s
		JOIN turns t ON t.session_id = s.id
		LEFT JOIN outcomes o ON o.turn_id = t.id
		WHERE (? = '' OR s.repo_path = ?)`, repoPath, repoPath)
	if err != nil {
		return nil, fmt.Errorf("learning: query session turns: %w", err)
	}
	var records []turnRecord
	for rows.Next() {
		var record turnRecord
		var sessionStarted, turnTS string
		var outcomeKind, userEdit, outcomeTS sql.NullString
		if err := rows.Scan(
			&record.repoPath, &sessionStarted, &record.turn.ID, &record.turn.SessionID, &record.turn.Role,
			&record.turn.Content, &turnTS, &outcomeKind, &userEdit, &outcomeTS,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("learning: scan session turn: %w", err)
		}
		record.turn.Timestamp, err = parseTimestamp(turnTS)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("learning: parse turn %q timestamp: %w", record.turn.ID, err)
		}
		if outcomeKind.Valid {
			at, err := parseTimestamp(outcomeTS.String)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("learning: parse outcome for turn %q timestamp: %w", record.turn.ID, err)
			}
			record.outcome = &Outcome{TurnID: record.turn.ID, Kind: outcomeKind.String, UserEdit: userEdit.String, Timestamp: at}
		}
		record.sessionStarted, err = parseTimestamp(sessionStarted)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("learning: parse session %q start timestamp: %w", record.turn.SessionID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("learning: iterate session turns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("learning: close session turns: %w", err)
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].sessionStarted.Equal(records[j].sessionStarted) {
			return records[i].sessionStarted.Before(records[j].sessionStarted)
		}
		if records[i].turn.SessionID != records[j].turn.SessionID {
			return records[i].turn.SessionID < records[j].turn.SessionID
		}
		if !records[i].turn.Timestamp.Equal(records[j].turn.Timestamp) {
			return records[i].turn.Timestamp.Before(records[j].turn.Timestamp)
		}
		return records[i].turn.ID < records[j].turn.ID
	})

	callsByTurn, err := s.toolCallsByTurn(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	var segments []SessionSegment
	var current *SessionSegment
	flush := func() {
		if current != nil {
			segments = append(segments, *current)
			current = nil
		}
	}
	for _, record := range records {
		if current != nil && record.turn.SessionID != current.SessionID {
			flush()
		}
		if record.turn.Role == "user" {
			flush()
			current = &SessionSegment{SessionID: record.turn.SessionID, RepoPath: record.repoPath, Trigger: record.turn}
		}
		if current == nil {
			continue
		}
		current.Turns = append(current.Turns, record.turn)
		current.ToolCalls = append(current.ToolCalls, callsByTurn[record.turn.ID]...)
		if record.outcome != nil {
			outcome := *record.outcome
			current.Outcome = &outcome
		}
	}
	flush()
	return segments, nil
}

func (s *SQLStore) toolCallsByTurn(ctx context.Context, repoPath string) (map[string][]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tc.id, tc.turn_id, tc.name, tc.input, tc.output, tc.duration_ms, tc.ts
		FROM tool_calls tc
		JOIN turns t ON t.id = tc.turn_id
		JOIN sessions s ON s.id = t.session_id
		WHERE (? = '' OR s.repo_path = ?)`, repoPath, repoPath)
	if err != nil {
		return nil, fmt.Errorf("learning: query segment tool calls: %w", err)
	}
	defer rows.Close()
	calls := make(map[string][]ToolCall)
	for rows.Next() {
		var call ToolCall
		var durationMS int64
		var ts string
		if err := rows.Scan(&call.ID, &call.TurnID, &call.Name, &call.Input, &call.Output, &durationMS, &ts); err != nil {
			return nil, fmt.Errorf("learning: scan segment tool call: %w", err)
		}
		call.Duration = time.Duration(durationMS) * time.Millisecond
		call.Timestamp, err = parseTimestamp(ts)
		if err != nil {
			return nil, fmt.Errorf("learning: parse tool call %q timestamp: %w", call.ID, err)
		}
		calls[call.TurnID] = append(calls[call.TurnID], call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: iterate segment tool calls: %w", err)
	}
	for turnID := range calls {
		sort.Slice(calls[turnID], func(i, j int) bool {
			if !calls[turnID][i].Timestamp.Equal(calls[turnID][j].Timestamp) {
				return calls[turnID][i].Timestamp.Before(calls[turnID][j].Timestamp)
			}
			return calls[turnID][i].ID < calls[turnID][j].ID
		})
	}
	return calls, nil
}

// SaveCandidates upserts absolute candidate state without incrementing
// previously persisted counts.
func (s *SQLStore) SaveCandidates(ctx context.Context, candidates []PatternCandidate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("learning: begin candidate persistence: %w", err)
	}
	defer tx.Rollback()
	for _, candidate := range candidates {
		sequence, err := json.Marshal(candidate.ToolSequence)
		if err != nil {
			return fmt.Errorf("learning: marshal candidate %q tool sequence: %w", candidate.TriggerHash, err)
		}
		var lastUsedAt any
		if !candidate.LastUsedAt.IsZero() {
			lastUsedAt = timestamp(candidate.LastUsedAt)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO patterns
				(repo_path, trigger_hash, solution_summary, tool_sequence, success_count, fail_count, last_used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo_path, trigger_hash) DO UPDATE SET
				solution_summary = excluded.solution_summary,
				tool_sequence = excluded.tool_sequence,
				success_count = excluded.success_count,
				fail_count = excluded.fail_count,
				last_used_at = excluded.last_used_at`,
			candidate.RepoPath, candidate.TriggerHash, candidate.SolutionSummary, string(sequence),
			candidate.SuccessCount, candidate.FailCount, lastUsedAt,
		); err != nil {
			return fmt.Errorf("learning: persist candidate %q: %w", candidate.TriggerHash, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("learning: commit candidate persistence: %w", err)
	}
	return nil
}

// Candidates returns persisted candidates in repository and trigger-hash order.
func (s *SQLStore) Candidates(ctx context.Context, repoPath string) ([]PatternCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repo_path, trigger_hash, solution_summary, tool_sequence,
			success_count, fail_count, last_used_at
		FROM patterns
		WHERE (? = '' OR repo_path = ?)
		ORDER BY repo_path, trigger_hash`, repoPath, repoPath)
	if err != nil {
		return nil, fmt.Errorf("learning: query candidates: %w", err)
	}
	defer rows.Close()
	var candidates []PatternCandidate
	for rows.Next() {
		var candidate PatternCandidate
		var sequence string
		var lastUsedAt sql.NullString
		if err := rows.Scan(
			&candidate.ID, &candidate.RepoPath, &candidate.TriggerHash, &candidate.SolutionSummary,
			&sequence, &candidate.SuccessCount, &candidate.FailCount, &lastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("learning: scan candidate: %w", err)
		}
		if err := json.Unmarshal([]byte(sequence), &candidate.ToolSequence); err != nil {
			return nil, fmt.Errorf("learning: parse candidate %q tool sequence: %w", candidate.TriggerHash, err)
		}
		if lastUsedAt.Valid {
			candidate.LastUsedAt, err = parseTimestamp(lastUsedAt.String)
			if err != nil {
				return nil, fmt.Errorf("learning: parse candidate %q last-used timestamp: %w", candidate.TriggerHash, err)
			}
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: iterate candidates: %w", err)
	}
	return candidates, nil
}

// ExtractCandidates deterministically derives and persists candidates for a repository.
func (s *SQLStore) ExtractCandidates(ctx context.Context, repoPath string) ([]PatternCandidate, error) {
	segments, err := s.Segments(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	if err := s.SaveCandidates(ctx, ClusterCandidates(segments, MinimumCandidateCount)); err != nil {
		return nil, err
	}
	return s.Candidates(ctx, repoPath)
}

// Stats returns aggregate row and outcome counts.
func (s *SQLStore) Stats(ctx context.Context) (*StatsReport, error) {
	var stats StatsReport
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM sessions),
			(SELECT count(*) FROM turns),
			(SELECT count(*) FROM tool_calls),
			count(*),
			(SELECT count(*) FROM patterns),
			coalesce(sum(kind = 'accepted'), 0),
			coalesce(sum(kind = 'rejected'), 0),
			coalesce(sum(kind = 'edited'), 0),
			coalesce(sum(kind = 'reverted'), 0)
		FROM outcomes`).Scan(
		&stats.Sessions, &stats.Turns, &stats.ToolCalls, &stats.Outcomes, &stats.Patterns,
		&stats.Accepted, &stats.Rejected, &stats.Edited, &stats.Reverted,
	)
	if err != nil {
		return nil, fmt.Errorf("learning: query SQL store statistics: %w", err)
	}
	return &stats, nil
}

// Close releases the database resources after observing ctx cancellation.
func (s *SQLStore) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("learning: close SQL store: %w", err)
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("learning: close SQL store: %w", err)
	}
	return nil
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func nullableTimestamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timestamp(*t)
}

func parseTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
