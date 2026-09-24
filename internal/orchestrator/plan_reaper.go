package orchestrator

import (
	"context"
	"time"
)

// ReapExpiredPlans parks abandoned plan nodes without replaying their effects.
// The underlying plan scan is bounded and remains internal to the supervisor.
func (o *Orchestrator) ReapExpiredPlans(ctx context.Context) (int, error) {
	if o == nil || o.planStore == nil {
		return 0, nil
	}
	return o.planStore.ReapExpiredLeases(ctx, time.Now().UTC(), 100)
}
