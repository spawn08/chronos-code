package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Executor is shared by all durable delivery workers. The context belongs to
// the worker service, not to the client that admitted the delivery.
type Executor interface {
	Execute(context.Context, *Execution) Outcome
}

type Execution struct {
	store *DeliveryStore
	Lease Lease
}

func (e *Execution) Checkpoint(ctx context.Context, value []byte) error {
	return e.store.Checkpoint(ctx, e.Lease, value)
}

func (e *Execution) PublishResult(ctx context.Context, value []byte) error {
	return e.store.PublishResult(ctx, e.Lease, value)
}

func (e *Execution) Transition(ctx context.Context, next DeliveryState) (Delivery, error) {
	return e.store.TransitionLease(ctx, e.Lease, next)
}

type WorkerConfig struct {
	OwnerID        string
	Concurrency    int
	LeaseDuration  time.Duration
	HeartbeatEvery time.Duration
	PollEvery      time.Duration
}

type Worker struct {
	store    *DeliveryStore
	executor Executor
	config   WorkerConfig

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	lastErr error
}

// LastError reports a non-idle worker failure without treating an empty queue
// or a deliberate shutdown as a readiness failure.
func (w *Worker) LastError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

func NewWorker(store *DeliveryStore, executor Executor, config WorkerConfig) (*Worker, error) {
	if store == nil || executor == nil || config.OwnerID == "" || config.Concurrency < 1 || config.LeaseDuration <= 0 || config.HeartbeatEvery <= 0 || config.HeartbeatEvery >= config.LeaseDuration || config.PollEvery <= 0 {
		return nil, ErrInvalidDelivery
	}
	return &Worker{store: store, executor: executor, config: config}, nil
}

// Start launches a bounded worker service. Stop cancels worker-owned execution
// contexts; interrupted leases are left to expire and can be reclaimed after a
// process restart.
func (w *Worker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return errors.New("delivery worker already started")
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	go w.run(ctx, w.done)
	return nil
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		w.mu.Lock()
		w.cancel, w.done = nil, nil
		w.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	var workers sync.WaitGroup
	workers.Add(w.config.Concurrency)
	for range w.config.Concurrency {
		go func() {
			defer workers.Done()
			for {
				err := w.RunOnce(ctx)
				if err == nil {
					w.mu.Lock()
					w.lastErr = nil
					w.mu.Unlock()
					continue
				}
				if ctx.Err() == nil && !errors.Is(err, ErrNoRunnableDelivery) {
					w.mu.Lock()
					w.lastErr = err
					w.mu.Unlock()
				}
				timer := time.NewTimer(w.config.PollEvery)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	workers.Wait()
}

// RunOnce claims and executes at most one delivery. It is also useful for
// deterministic service tests and supervisors that own their polling loop.
func (w *Worker) RunOnce(workerCtx context.Context) error {
	lease, err := w.store.Claim(workerCtx, w.config.OwnerID, w.config.LeaseDuration)
	if err != nil {
		return err
	}
	executionCtx, cancelExecution := context.WithCancel(workerCtx)
	defer cancelExecution()
	heartbeatDone := make(chan struct{})
	heartbeatErr := make(chan error, 1)
	stopHeartbeat := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(w.config.HeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-executionCtx.Done():
				return
			case <-ticker.C:
				if _, err := w.store.Heartbeat(executionCtx, lease, w.config.LeaseDuration); err != nil {
					heartbeatErr <- err
					cancelExecution()
					return
				}
			}
		}
	}()

	outcome := w.executor.Execute(executionCtx, &Execution{store: w.store, Lease: lease})
	close(stopHeartbeat)
	<-heartbeatDone
	select {
	case err := <-heartbeatErr:
		return err
	default:
	}
	if workerCtx.Err() != nil {
		return workerCtx.Err()
	}
	if _, err := w.store.Finalize(workerCtx, lease, outcome); err != nil {
		return fmt.Errorf("apply delivery executor outcome: %w", err)
	}
	return nil
}
