package config

// store_postgres_suppressions.go — CRUD + transaction-accepting variants
// for the purchase_suppressions table (migrations 000037, 000134) plus the
// WithTx helper used by executePurchase and cancelExecution to commit
// the execution row + suppression rows atomically.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WithTx opens a pgx transaction, runs fn, and commits on success or
// rolls back on error. Errors from fn are returned as-is; errors from
// Begin / Commit / Rollback are wrapped so callers can distinguish
// "user-code failed" from "transport failed".
func (s *PostgresStore) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, "WithTx")

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// rollbackTimeout bounds the deferred rollback, which runs on a context
// detached from the request so a canceled request still rolls back.
const rollbackTimeout = 5 * time.Second

// rollbackTx is the deferred best-effort rollback for a pgx transaction.
// After a successful Commit, Rollback returns pgx.ErrTxClosed, which is
// expected and stays quiet. Any other error is a genuine rollback failure
// (dropped connection, server-side abort) and is logged with op so it is
// visible. It is logged rather than returned so it never masks the error
// (or commit result) the caller already has.
//
// The rollback runs on context.WithoutCancel(ctx) with a short timeout: a
// canceled request ctx is the usual reason a tx body fails, and rolling
// back on it would fail with a context error instead of rolling back.
func rollbackTx(ctx context.Context, tx pgx.Tx, op string) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := tx.Rollback(rbCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		logging.Warnf("%s: transaction rollback failed: %v", op, err)
	}
}

// CreateSuppression inserts a suppression row using a one-call
// transaction. Callers that need to bundle this write with other
// statements (e.g. the matching execution insert) should use
// CreateSuppressionTx inside a WithTx block instead.
func (s *PostgresStore) CreateSuppression(ctx context.Context, sup *PurchaseSuppression) error {
	// Validate before opening the tx so a trivially-bad request
	// doesn't incur the round-trip to Postgres' BEGIN.
	if err := validateSuppression(sup); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.CreateSuppressionTx(ctx, tx, sup)
	})
}

// validateSuppression enforces the invariants required by CreateSuppression /
// CreateSuppressionTx: non-nil pointer, non-empty execution ID, positive
// count, non-zero expiry.
func validateSuppression(sup *PurchaseSuppression) error {
	if sup == nil {
		return errors.New("suppression is nil")
	}
	if sup.ExecutionID == "" {
		return errors.New("suppression.ExecutionID is required")
	}
	if sup.SuppressedCount <= 0 {
		return fmt.Errorf("suppression.SuppressedCount must be > 0, got %d", sup.SuppressedCount)
	}
	if sup.ExpiresAt.IsZero() {
		return errors.New("suppression.ExpiresAt is required")
	}
	return nil
}

// CreateSuppressionTx inserts a suppression row inside a caller-owned
// transaction. sup.ID is populated from the Postgres-generated UUID if
// the caller left it blank.
func (s *PostgresStore) CreateSuppressionTx(ctx context.Context, tx pgx.Tx, sup *PurchaseSuppression) error {
	if err := validateSuppression(sup); err != nil {
		return err
	}
	if sup.ID == "" {
		sup.ID = uuid.New().String()
	}

	// A nil Variant writes NULL into all three variant columns (a legacy,
	// 6-tuple-wide row); otherwise all three are written, empty strings included
	// (migration 000134 keeps them all-NULL or all-set).
	var az, platform, tenancy *string
	if sup.Variant != nil {
		az, platform, tenancy = &sup.Variant.AvailabilityZone, &sup.Variant.Platform, &sup.Variant.Tenancy
	}
	const q = `
		INSERT INTO purchase_suppressions (
			id, execution_id, account_id, provider, service, region,
			resource_type, engine, suppressed_count, expires_at,
			availability_zone, platform, tenancy
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`
	if _, err := tx.Exec(ctx, q,
		sup.ID, sup.ExecutionID, sup.AccountID, sup.Provider, sup.Service,
		sup.Region, sup.ResourceType, sup.Engine, sup.SuppressedCount,
		sup.ExpiresAt, az, platform, tenancy,
	); err != nil {
		return fmt.Errorf("failed to insert purchase suppression: %w", err)
	}
	return nil
}

// DeleteSuppressionsByExecution deletes all suppression rows for the
// given execution using a one-call transaction. Used by the cancel
// path when the whole execution's capacity should be un-suppressed.
func (s *PostgresStore) DeleteSuppressionsByExecution(ctx context.Context, executionID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.DeleteSuppressionsByExecutionTx(ctx, tx, executionID)
	})
}

// DeleteSuppressionsByExecutionTx deletes all suppression rows for the
// given execution inside a caller-owned transaction. Safe to call on
// an execution with no suppression rows (e.g. grace_period_days=0 at
// insert time) — returns nil, no error.
func (s *PostgresStore) DeleteSuppressionsByExecutionTx(ctx context.Context, tx pgx.Tx, executionID string) error {
	if executionID == "" {
		return errors.New("executionID is required")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM purchase_suppressions WHERE execution_id = $1`, executionID); err != nil {
		return fmt.Errorf("failed to delete purchase suppressions: %w", err)
	}
	return nil
}

// ListActiveSuppressions returns every suppression row whose expiry
// hasn't passed. The scheduler groups these by the 6-tuple key
// (account, provider, service, region, resource_type, engine) and
// matches each row's variant (availability zone, platform, tenancy;
// issue #816) to subtract cumulative suppressed counts from its rec-list
// output. A row with NULL variant columns comes back with a nil Variant.
//
// Deliberately unbounded (#120 review): every active row subtracts from a
// rec's count, and one purchase can write up to 1000 rows that live 7-90
// days, so normal use can exceed MaxListLimit. A capped read would either
// under-suppress (re-offering capacity just bought) or fail every caller.
func (s *PostgresStore) ListActiveSuppressions(ctx context.Context) ([]PurchaseSuppression, error) {
	const q = `
		SELECT id, execution_id, account_id, provider, service, region,
		       resource_type, engine, suppressed_count, expires_at, created_at,
		       availability_zone, platform, tenancy
		FROM purchase_suppressions
		WHERE expires_at > NOW()
	`
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query active suppressions: %w", err)
	}
	defer rows.Close()

	var out []PurchaseSuppression
	for rows.Next() {
		var sup PurchaseSuppression
		var az, platform, tenancy *string
		if err := rows.Scan(
			&sup.ID, &sup.ExecutionID, &sup.AccountID, &sup.Provider,
			&sup.Service, &sup.Region, &sup.ResourceType, &sup.Engine,
			&sup.SuppressedCount, &sup.ExpiresAt, &sup.CreatedAt,
			&az, &platform, &tenancy,
		); err != nil {
			return nil, fmt.Errorf("failed to scan suppression row: %w", err)
		}
		sup.Variant = suppressionVariantFromColumns(az, platform, tenancy)
		out = append(out, sup)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate suppression rows: %w", err)
	}
	return out, nil
}

// suppressionVariantFromColumns rebuilds a row's variant from its nullable
// columns. The 000134 CHECK keeps them all-NULL or all-set; a row with any
// NULL is read as having no variant (a 6-tuple-wide wildcard), which
// over-suppresses rather than over-shows.
func suppressionVariantFromColumns(az, platform, tenancy *string) *SuppressionVariant {
	if az == nil || platform == nil || tenancy == nil {
		return nil
	}
	return &SuppressionVariant{AvailabilityZone: *az, Platform: *platform, Tenancy: *tenancy}
}
