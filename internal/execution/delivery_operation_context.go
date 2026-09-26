package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type operationLeaseKey struct{}

type OperationLease struct {
	Store *DeliveryStore
	Lease Lease
}

func (e *Execution) OperationContext(ctx context.Context) context.Context {
	return WithOperationLease(ctx, e.store, e.Lease)
}

func (e *Execution) PriorOperations(ctx context.Context) ([]Operation, error) {
	return e.store.Operations(ctx, e.Lease.Delivery.DeliveryScope, e.Lease.Delivery.ID)
}

func (e *Execution) CoversOperation(ctx context.Context, op Operation) (bool, error) {
	return e.store.CoversOperation(ctx, e.Lease, op)
}

func (e *Execution) CheckpointedCallCount(ctx context.Context) (int64, error) {
	return e.store.CheckpointedCallCount(ctx, e.Lease)
}

func (e *Execution) CheckpointedCallsByNode(ctx context.Context) (map[string]int64, error) {
	return e.store.CheckpointedCallsByNode(ctx, e.Lease)
}

func (e *Execution) PriorAttempts(ctx context.Context) ([]AttemptRecord, error) {
	return e.store.Attempts(ctx, e.Lease.Delivery.DeliveryScope, e.Lease.Delivery.ID)
}

func (e *Execution) WithLeaseEffect(ctx context.Context, duration time.Duration, effect func(context.Context) error) error {
	return e.store.WithLeaseEffect(ctx, e.Lease, duration, effect)
}

// ReconcileOperation accepts only host-observed evidence under the current
// worker lease; tool/model arguments cannot invoke this method.
func (e *Execution) ReconcileOperation(ctx context.Context, id, proof, fingerprint string, result json.RawMessage) (Operation, error) {
	return e.store.ReconcileOperation(ctx, e.Lease, id, proof, fingerprint, result)
}

func (e *Execution) CumulativeUsage(ctx context.Context) (CumulativeUsage, error) {
	return e.store.Usage(ctx, e.Lease.Delivery.DeliveryScope, e.Lease.Delivery.ID)
}

func (e *Execution) UsageByNode(ctx context.Context) (map[string]NodeCallCounts, error) {
	return e.store.UsageByNode(ctx, e.Lease.Delivery.DeliveryScope, e.Lease.Delivery.ID)
}

// WithOperationLease is for host-owned workers and bounded integration tests;
// model tool arguments cannot manufacture a worker lease.
func WithOperationLease(ctx context.Context, store *DeliveryStore, lease Lease) context.Context {
	return context.WithValue(ctx, operationLeaseKey{}, OperationLease{Store: store, Lease: lease})
}

func OperationLeaseFromContext(ctx context.Context) (OperationLease, bool) {
	lease, ok := ctx.Value(operationLeaseKey{}).(OperationLease)
	return lease, ok && lease.Store != nil && lease.Lease.Delivery.ID != ""
}

// EffectJournalError prevents the SDK from presenting an ambiguous effect as
// a recoverable tool error that the model might retry.
type EffectJournalError struct{ Err error }

func (e EffectJournalError) Error() string { return fmt.Sprintf("effect journal: %v", e.Err) }
func (e EffectJournalError) Unwrap() error { return e.Err }
func (e EffectJournalError) FatalEffect()  {}
