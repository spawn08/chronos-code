package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ledgerRecordVersion identifies the on-disk encoding of one ledger event.
const ledgerRecordVersion = 1

// ErrCorruptLedger reports durable ledger history that cannot be replayed.
var ErrCorruptLedger = errors.New("corrupt evidence ledger")

// EventSink durably records an event before the ledger commits it in memory.
// A sink error rejects the event, so persisted history is never behind the
// in-memory view.
type EventSink interface {
	AppendEvent(event Event) error
}

// AttachSink sets the sink used for subsequent appends. Events already in the
// ledger are not re-sent; callers attach after replaying durable history.
func (l *Ledger) AttachSink(sink EventSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sink = sink
}

type ledgerRecord struct {
	Version int   `json:"v"`
	Event   Event `json:"event"`
}

// FileLedgerStore persists each task's events as append-only JSON lines so
// evidence survives across executions, process restarts, and resumes of the
// same task ID.
type FileLedgerStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileLedgerStore stores ledgers under dir, creating it on first write.
func NewFileLedgerStore(dir string) *FileLedgerStore {
	return &FileLedgerStore{dir: dir}
}

// Path returns the file that holds taskID's history. Task IDs are hashed so
// arbitrary identifiers cannot escape the store directory.
func (s *FileLedgerStore) Path(taskID TaskID) string {
	return filepath.Join(s.dir, fmt.Sprintf("%x.jsonl", sha256.Sum256([]byte(taskID))))
}

// Load returns the ordered durable events for taskID. A missing file is an
// empty history. A torn final record (a crash mid-append) is truncated away;
// any other malformed or out-of-order history is ErrCorruptLedger.
func (s *FileLedgerStore) Load(taskID TaskID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(taskID)
}

func (s *FileLedgerStore) loadLocked(taskID TaskID) ([]Event, error) {
	path := s.Path(taskID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read evidence ledger: %w", err)
	}
	var events []Event
	offset := 0
	for offset < len(data) {
		end := bytes.IndexByte(data[offset:], '\n')
		if end < 0 {
			// Unterminated tail: the append never completed. Drop it so the
			// next append does not concatenate onto a partial record.
			if err := os.Truncate(path, int64(offset)); err != nil {
				return nil, fmt.Errorf("truncate torn evidence ledger record: %w", err)
			}
			break
		}
		line := data[offset : offset+end]
		offset += end + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record ledgerRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("%w: record %d: %v", ErrCorruptLedger, len(events)+1, err)
		}
		if record.Version != ledgerRecordVersion {
			return nil, fmt.Errorf("%w: record %d: unsupported version %d", ErrCorruptLedger, len(events)+1, record.Version)
		}
		if record.Event.TaskID != taskID {
			return nil, fmt.Errorf("%w: record %d: %w", ErrCorruptLedger, len(events)+1, ErrTaskMismatch)
		}
		events = append(events, record.Event)
	}
	if _, err := Reduce(events); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptLedger, err)
	}
	return events, nil
}

// Open replays taskID's durable history into a new ledger and attaches a sink
// so every later event is persisted before it is committed.
func (s *FileLedgerStore) Open(taskID TaskID) (*Ledger, error) {
	if taskID == "" {
		return nil, fmt.Errorf("open evidence ledger: task ID is required")
	}
	events, err := s.Load(taskID)
	if err != nil {
		return nil, err
	}
	ledger := NewLedger(taskID)
	for _, event := range events {
		if err := ledger.Append(event); err != nil {
			return nil, fmt.Errorf("replay evidence ledger: %w", err)
		}
	}
	ledger.AttachSink(&fileLedgerSink{store: s, taskID: taskID})
	return ledger, nil
}

type fileLedgerSink struct {
	store  *FileLedgerStore
	taskID TaskID
}

func (f *fileLedgerSink) AppendEvent(event Event) error {
	line, err := json.Marshal(ledgerRecord{Version: ledgerRecordVersion, Event: event})
	if err != nil {
		return fmt.Errorf("encode evidence event: %w", err)
	}
	line = append(line, '\n')

	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if err := os.MkdirAll(f.store.dir, 0o700); err != nil {
		return fmt.Errorf("create evidence ledger directory: %w", err)
	}
	file, err := os.OpenFile(f.store.Path(f.taskID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open evidence ledger: %w", err)
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return fmt.Errorf("append evidence ledger: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync evidence ledger: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close evidence ledger: %w", err)
	}
	return nil
}
