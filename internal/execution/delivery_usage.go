package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

var (
	ErrUsageConflict       = errors.New("delivery usage call identity conflict")
	ErrUnknownPricing      = errors.New("trusted model pricing is required by delivery cost authority")
	ErrUsageOutcomeUnknown = errors.New("delivery usage outcome needs reconciliation")
	ErrCostAuthority       = errors.New("delivery cost authority reached")
)

type UsageReservation struct {
	CallID               string
	Provider             string
	Model                string
	EstimateTokens       int64
	ReservedMicrodollars int64
	KnownPrice           bool
	Status               string
}

type IncurredUsage struct {
	InputTokens          int64
	OutputTokens         int64
	CacheReadTokens      int64
	CacheCreationTokens  int64
	CostMicrodollars     int64
	CostKnown            bool
	ActiveNanoseconds    int64
}

type CumulativeUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadTokens      int64 `json:"cache_read_tokens"`
	CacheCreationTokens  int64 `json:"cache_creation_tokens"`
	SpentMicrodollars    int64 `json:"spent_microdollars"`
	ReservedMicrodollars int64 `json:"reserved_microdollars"`
	ActiveNanoseconds    int64 `json:"active_nanoseconds"`
	CostKnown            bool  `json:"cost_known"`
	UnknownCalls         int64 `json:"unknown_calls"`
}

// ReserveUsage records a stable call before contacting a provider. Competing
// children share the same durable cap; no worker may reset it on a new window.
func (s *DeliveryStore) ReserveUsage(ctx context.Context, scope DeliveryScope, id DeliveryID, request UsageReservation) (UsageReservation, error) {
	if err := validateDeliveryRef(scope, id); err != nil || request.CallID == "" || request.Provider == "" || request.Model == "" || request.EstimateTokens < 0 || request.ReservedMicrodollars < 0 || !request.KnownPrice && request.ReservedMicrodollars != 0 {
		return UsageReservation{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageReservation{}, fmt.Errorf("begin usage reservation: %w", err)
	}
	defer tx.Rollback()
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return UsageReservation{}, err
	}
	existing, err := loadUsageReservation(ctx, tx, scope, id, request.CallID)
	if err == nil {
		if existing.Provider != request.Provider || existing.Model != request.Model || existing.EstimateTokens != request.EstimateTokens || existing.ReservedMicrodollars != request.ReservedMicrodollars || existing.KnownPrice != request.KnownPrice {
			return UsageReservation{}, ErrUsageConflict
		}
		if existing.Status == "unknown" {
			return existing, ErrUsageOutcomeUnknown
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return UsageReservation{}, err
	}
	usage, err := loadCumulativeUsage(ctx, tx, scope, id)
	if err != nil {
		return UsageReservation{}, err
	}
	if delivery.MaxCostMicrodollars > 0 {
		if !request.KnownPrice || request.EstimateTokens > 0 && request.ReservedMicrodollars == 0 {
			return UsageReservation{}, ErrUnknownPricing
		}
		if usage.UnknownCalls > 0 {
			return UsageReservation{}, ErrUsageOutcomeUnknown
		}
		remaining := delivery.MaxCostMicrodollars - usage.SpentMicrodollars
		if remaining < 0 || request.ReservedMicrodollars > remaining || usage.ReservedMicrodollars > remaining-request.ReservedMicrodollars {
			return UsageReservation{}, ErrCostAuthority
		}
	}
	request.Status = "reserved"
	now := timestamp(s.clock.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_usage_calls (tenant_id, repository_id, delivery_id, call_id, status, provider, model, estimate_tokens, reserved_microdollars, known_price, prepared_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.RepositoryID, id, request.CallID, request.Status, request.Provider, request.Model, request.EstimateTokens, request.ReservedMicrodollars, request.KnownPrice, now); err != nil {
		return UsageReservation{}, fmt.Errorf("persist usage reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UsageReservation{}, fmt.Errorf("commit usage reservation: %w", err)
	}
	return request, nil
}

// ReconcileUsage records actual incurred usage even when it exceeds the
// estimate or an explicit cap. Repeated completions cannot count twice.
func (s *DeliveryStore) ReconcileUsage(ctx context.Context, scope DeliveryScope, id DeliveryID, callID string, actual IncurredUsage) (CumulativeUsage, error) {
	if err := validateDeliveryRef(scope, id); err != nil || callID == "" || actual.InputTokens < 0 || actual.OutputTokens < 0 || actual.CacheReadTokens < 0 || actual.CacheCreationTokens < 0 || actual.CostMicrodollars < 0 || actual.ActiveNanoseconds < 0 || !actual.CostKnown && actual.CostMicrodollars != 0 {
		return CumulativeUsage{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CumulativeUsage{}, fmt.Errorf("begin usage reconciliation: %w", err)
	}
	defer tx.Rollback()
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return CumulativeUsage{}, err
	}
	reserved, err := loadUsageReservation(ctx, tx, scope, id, callID)
	if err != nil {
		return CumulativeUsage{}, err
	}
	if delivery.MaxCostMicrodollars > 0 && !actual.CostKnown {
		return CumulativeUsage{}, ErrUnknownPricing
	}
	if reserved.Status == "cancelled" {
		return CumulativeUsage{}, ErrUsageConflict
	}
	if reserved.Status == "reconciled" {
		var prior IncurredUsage
		var costKnown bool
		if err := tx.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, actual_microdollars, actual_cost_known, active_nanoseconds FROM delivery_usage_calls WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND call_id = ?`, scope.TenantID, scope.RepositoryID, id, callID).Scan(&prior.InputTokens, &prior.OutputTokens, &prior.CacheReadTokens, &prior.CacheCreationTokens, &prior.CostMicrodollars, &costKnown, &prior.ActiveNanoseconds); err != nil {
			return CumulativeUsage{}, fmt.Errorf("read reconciled usage: %w", err)
		}
		prior.CostKnown = costKnown
		if prior != actual {
			return CumulativeUsage{}, ErrUsageConflict
		}
		usage, err := loadCumulativeUsage(ctx, tx, scope, id)
		if err == nil && delivery.MaxCostMicrodollars > 0 && usage.SpentMicrodollars > delivery.MaxCostMicrodollars {
			return usage, ErrCostAuthority
		}
		return usage, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_usage_calls SET status = 'reconciled', input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_creation_tokens = ?, actual_microdollars = ?, actual_cost_known = ?, active_nanoseconds = ?, reconciled_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND call_id = ?`, actual.InputTokens, actual.OutputTokens, actual.CacheReadTokens, actual.CacheCreationTokens, actual.CostMicrodollars, actual.CostKnown, actual.ActiveNanoseconds, timestamp(s.clock.Now()), scope.TenantID, scope.RepositoryID, id, callID); err != nil {
		return CumulativeUsage{}, fmt.Errorf("persist actual delivery usage: %w", err)
	}
	usage, err := loadCumulativeUsage(ctx, tx, scope, id)
	if err != nil {
		return CumulativeUsage{}, err
	}
	if err := tx.Commit(); err != nil {
		return CumulativeUsage{}, fmt.Errorf("commit actual delivery usage: %w", err)
	}
	if delivery.MaxCostMicrodollars > 0 && usage.SpentMicrodollars > delivery.MaxCostMicrodollars {
		return usage, ErrCostAuthority
	}
	return usage, nil
}

// MarkUsageUnknown retains the reservation when a provider may have billed but
// no trustworthy usage was returned. A capped delivery cannot admit more work.
func (s *DeliveryStore) MarkUsageUnknown(ctx context.Context, scope DeliveryScope, id DeliveryID, callID string) error {
	if err := validateDeliveryRef(scope, id); err != nil || callID == "" {
		return ErrInvalidDelivery
	}
	result, err := s.db.ExecContext(ctx, `UPDATE delivery_usage_calls SET status = 'unknown' WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND call_id = ? AND status IN ('reserved','unknown')`, scope.TenantID, scope.RepositoryID, id, callID)
	if err != nil {
		return fmt.Errorf("mark delivery usage unknown: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count unknown usage: %w", err)
	}
	if changed != 1 {
		return ErrUsageConflict
	}
	return nil
}

// CancelUnusedReservation is only valid when the host knows no provider call
// was sent; uncertain requests must use MarkUsageUnknown instead.
func (s *DeliveryStore) CancelUnusedReservation(ctx context.Context, scope DeliveryScope, id DeliveryID, callID string) error {
	if err := validateDeliveryRef(scope, id); err != nil || callID == "" {
		return ErrInvalidDelivery
	}
	result, err := s.db.ExecContext(ctx, `UPDATE delivery_usage_calls SET status = 'cancelled' WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND call_id = ? AND status = 'reserved'`, scope.TenantID, scope.RepositoryID, id, callID)
	if err != nil {
		return fmt.Errorf("cancel unsent provider reservation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cancelled reservation: %w", err)
	}
	if changed != 1 {
		return ErrUsageConflict
	}
	return nil
}

func (s *DeliveryStore) Usage(ctx context.Context, scope DeliveryScope, id DeliveryID) (CumulativeUsage, error) {
	if err := validateDeliveryRef(scope, id); err != nil {
		return CumulativeUsage{}, err
	}
	if _, err := s.Load(ctx, scope, id); err != nil {
		return CumulativeUsage{}, err
	}
	return loadCumulativeUsage(ctx, s.db, scope, id)
}

func loadUsageReservation(ctx context.Context, db queryer, scope DeliveryScope, id DeliveryID, callID string) (UsageReservation, error) {
	var request UsageReservation
	err := db.QueryRowContext(ctx, `SELECT status, provider, model, estimate_tokens, reserved_microdollars, known_price FROM delivery_usage_calls WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND call_id = ?`, scope.TenantID, scope.RepositoryID, id, callID).Scan(&request.Status, &request.Provider, &request.Model, &request.EstimateTokens, &request.ReservedMicrodollars, &request.KnownPrice)
	if err != nil {
		return UsageReservation{}, err
	}
	request.CallID = callID
	return request, nil
}

func loadCumulativeUsage(ctx context.Context, db queryer, scope DeliveryScope, id DeliveryID) (CumulativeUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT status, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, actual_microdollars, active_nanoseconds, reserved_microdollars, known_price, actual_cost_known FROM delivery_usage_calls WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, scope.TenantID, scope.RepositoryID, id)
	if err != nil {
		return CumulativeUsage{}, fmt.Errorf("read delivery usage: %w", err)
	}
	defer rows.Close()
	usage := CumulativeUsage{CostKnown: true}
	for rows.Next() {
		var status string
		var input, output, cacheRead, cacheCreation, cost, active, reserved int64
		var knownPrice, actualKnown bool
		if err := rows.Scan(&status, &input, &output, &cacheRead, &cacheCreation, &cost, &active, &reserved, &knownPrice, &actualKnown); err != nil {
			return CumulativeUsage{}, fmt.Errorf("scan delivery usage: %w", err)
		}
		if status == "unknown" {
			usage.UnknownCalls++
			usage.CostKnown = false
		}
		if status == "reserved" || status == "unknown" {
			if !knownPrice {
				usage.CostKnown = false
			}
			if reserved > math.MaxInt64-usage.ReservedMicrodollars {
				return CumulativeUsage{}, ErrInvalidDelivery
			}
			usage.ReservedMicrodollars += reserved
		}
		if status != "reconciled" {
			continue
		}
		for _, item := range []struct{ value, sum *int64 }{{&input, &usage.InputTokens}, {&output, &usage.OutputTokens}, {&cacheRead, &usage.CacheReadTokens}, {&cacheCreation, &usage.CacheCreationTokens}, {&cost, &usage.SpentMicrodollars}, {&active, &usage.ActiveNanoseconds}} {
			if *item.value < 0 || *item.value > math.MaxInt64-*item.sum {
				return CumulativeUsage{}, ErrInvalidDelivery
			}
			*item.sum += *item.value
		}
		if !actualKnown {
			usage.CostKnown = false
		}
	}
	if err := rows.Err(); err != nil {
		return CumulativeUsage{}, fmt.Errorf("list delivery usage: %w", err)
	}
	return usage, nil
}
