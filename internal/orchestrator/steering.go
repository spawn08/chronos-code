package orchestrator

import (
	"context"
	"strings"
	"sync"
)

// steeringPreamble frames input added to a running task so the model folds it
// into the work in progress instead of treating it as a replacement request.
const steeringPreamble = "The user sent the following while you were working on the current task. " +
	"Treat it as additional context or instructions for that task and continue it. " +
	"It replaces the task only if it explicitly says so."

// SteeringInbox collects user messages submitted while one task is running.
// It implements agent.PendingInput: the agent loop drains it at tool-round
// boundaries and the drained text joins the running task. Close ends the
// task's window and returns whatever the loop never picked up, so the client
// can send it as the next turn. Every message is either delivered or returned
// by Close, never both.
type SteeringInbox struct {
	mu        sync.Mutex
	pending   []string
	delivered []string
	closed    bool
	signal    chan struct{}
}

// NewSteeringInbox returns an open, empty inbox.
func NewSteeringInbox() *SteeringInbox {
	return &SteeringInbox{signal: make(chan struct{}, 1)}
}

// Push adds text for the running task. It returns false when the inbox is
// closed or text is blank; the caller then owns the message.
func (b *SteeringInbox) Push(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.pending = append(b.pending, text)
	return true
}

// Pending returns a copy of the messages not yet delivered.
func (b *SteeringInbox) Pending() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.pending...)
}

// DrainPendingInput implements agent.PendingInput. It delivers every pending
// message as one framed user message unless ctx is done or the inbox closed.
func (b *SteeringInbox) DrainPendingInput(ctx context.Context) []string {
	if ctx.Err() != nil {
		return nil
	}
	b.mu.Lock()
	if b.closed || len(b.pending) == 0 {
		b.mu.Unlock()
		return nil
	}
	texts := b.pending
	b.pending = nil
	b.delivered = append(b.delivered, texts...)
	b.mu.Unlock()
	select {
	case b.signal <- struct{}{}:
	default: // A notification is already waiting; it covers this batch too.
	}
	return []string{"<user_update>\n" + steeringPreamble + "\n\n" + strings.Join(texts, "\n\n") + "\n</user_update>"}
}

// Delivered is signaled after a drain; call TakeDelivered to read the batch.
// Signals coalesce, so one receive may cover several drains.
func (b *SteeringInbox) Delivered() <-chan struct{} {
	return b.signal
}

// TakeDelivered returns and clears the raw text delivered since the last call.
func (b *SteeringInbox) TakeDelivered() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	texts := b.delivered
	b.delivered = nil
	return texts
}

// Close stops further delivery and returns the messages never delivered.
// Later drains return nothing and later pushes are refused.
func (b *SteeringInbox) Close() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	texts := b.pending
	b.pending = nil
	return texts
}
