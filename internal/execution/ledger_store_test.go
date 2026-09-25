package execution

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFileLedgerStoreReplaysEvidenceAcrossReopen(t *testing.T) {
	store := NewFileLedgerStore(t.TempDir())
	first, err := store.Open("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Record(Event{Type: EventWrite, Paths: []string{"main.go"}, ContentHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	check, err := first.Record(Event{Type: EventVerification, Paths: []string{"main.go"}, Passed: true, Command: "go test ./...", CommandClass: CommandTest, ExitCode: &exitCode, TerminalState: TerminalExited, CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	second, err := store.Open("task-1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := second.State()
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := state.Verification[check.EvidenceID]
	if !ok || !evidence.Current || evidence.Event.ExitCode == nil || *evidence.Event.ExitCode != 0 {
		t.Fatalf("replayed evidence = %#v, ok=%v", evidence, ok)
	}
	if second.Revision() != 1 {
		t.Fatalf("replayed revision = %d, want 1", second.Revision())
	}

	// Identity and revisions continue after replay; a later write still
	// invalidates evidence that was recorded by the earlier process.
	write, err := second.Record(Event{Type: EventWrite, Paths: []string{"main.go"}, ContentHash: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	if write.ID != "task-1:3" || write.Sequence != 3 || write.MutationRevision != 2 {
		t.Fatalf("continued event = %#v", write)
	}
	third, err := store.Open("task-1")
	if err != nil {
		t.Fatal(err)
	}
	state, err = third.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.Verification[check.EvidenceID].Current {
		t.Fatal("evidence invalidated before restart must stay stale after reopen")
	}
	if len(state.Events) != 3 {
		t.Fatalf("events after reopen = %d, want 3", len(state.Events))
	}
}

func TestFileLedgerStoreIsolatesTasks(t *testing.T) {
	store := NewFileLedgerStore(t.TempDir())
	a, err := store.Open("task-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Record(Event{Type: EventWrite, Paths: []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}
	b, err := store.Open("task-b")
	if err != nil {
		t.Fatal(err)
	}
	if events := b.Events(); len(events) != 0 {
		t.Fatalf("task-b events = %#v, want none", events)
	}
	if store.Path("../escape") == store.Path("task-a") || strings.Contains(store.Path("../escape"), "..") {
		t.Fatalf("task path is not a contained hash: %s", store.Path("../escape"))
	}
}

func TestFileLedgerStoreTruncatesTornTail(t *testing.T) {
	store := NewFileLedgerStore(t.TempDir())
	ledger, err := store.Open("task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(store.Path("task"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"v":1,"event":{"ID":"task:2"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open("task")
	if err != nil {
		t.Fatalf("torn tail must be recoverable: %v", err)
	}
	if _, err := reopened.Record(Event{Type: EventWrite, Paths: []string{"b.go"}}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Load("task")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Paths[0] != "b.go" {
		t.Fatalf("events after torn-tail repair = %#v", events)
	}
}

func TestFileLedgerStoreRejectsCorruptHistory(t *testing.T) {
	store := NewFileLedgerStore(t.TempDir())
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path("task"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("task"); !errors.Is(err, ErrCorruptLedger) {
		t.Fatalf("Open() error = %v, want %v", err, ErrCorruptLedger)
	}
}

type failingSink struct{}

func (failingSink) AppendEvent(Event) error { return errors.New("disk full") }

func TestLedgerSinkFailureDoesNotCommit(t *testing.T) {
	ledger := NewLedger("task")
	ledger.AttachSink(failingSink{})
	if _, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"a.go"}}); err == nil {
		t.Fatal("Record() succeeded despite sink failure")
	}
	if len(ledger.Events()) != 0 || ledger.Revision() != 0 {
		t.Fatalf("failed persistence committed state: events=%d revision=%d", len(ledger.Events()), ledger.Revision())
	}
	ledger.AttachSink(nil)
	event, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != "task:1" {
		t.Fatalf("identity advanced on failed persistence: %s", event.ID)
	}
}
