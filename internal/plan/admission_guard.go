package plan

import "context"

type admissionGuardKey struct{}

// AdmissionGuard is a host-owned transaction boundary for claiming a plan
// node under its parent delivery lease. It must execute the claim exactly once
// while the parent owner remains fenced against cancellation/reclaim.
type AdmissionGuard func(context.Context, func(context.Context) error) error

func WithAdmissionGuard(ctx context.Context, guard AdmissionGuard) context.Context {
	return context.WithValue(ctx, admissionGuardKey{}, guard)
}

func admissionGuardFromContext(ctx context.Context) AdmissionGuard {
	guard, _ := ctx.Value(admissionGuardKey{}).(AdmissionGuard)
	return guard
}
