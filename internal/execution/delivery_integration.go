package execution

import (
	"context"
	"fmt"
	"time"
)

// WithLeaseEffect holds the delivery store's write lock across one bounded
// external integration. The claim transaction cannot supersede this epoch
// until the effect's journal has been persisted or the effect has stopped.
// Callers must retain a separate effect journal because a git apply and SQLite
// cannot be one atomic transaction.
func (s *DeliveryStore) WithLeaseEffect(ctx context.Context, lease Lease, maxDuration time.Duration, effect func(context.Context) error) error {
	if maxDuration <= 0 || effect == nil {
		return ErrInvalidDelivery
	}
	effectCtx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	tx, err := s.db.BeginTx(effectCtx, nil)
	if err != nil {
		return fmt.Errorf("begin delivery integration fence: %w", err)
	}
	defer tx.Rollback()
	now := s.clock.Now().UTC()
	updated, err := tx.ExecContext(effectCtx, `UPDATE delivery_queue SET lease_expires_at = ?, lease_heartbeat_at = ?, updated_at = ?
	WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND status = 'leased' AND lease_owner_id = ? AND lease_epoch = ? AND lease_expires_at > ?`,
		timestamp(now.Add(maxDuration)), timestamp(now), timestamp(now), lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.OwnerID, lease.Epoch, timestamp(now))
	if err != nil {
		return fmt.Errorf("fence delivery integration: %w", err)
	}
	if err := requireChanged(updated); err != nil {
		return err
	}
	if err := effect(effectCtx); err != nil {
		return fmt.Errorf("apply fenced delivery integration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delivery integration fence: %w", err)
	}
	return nil
}
