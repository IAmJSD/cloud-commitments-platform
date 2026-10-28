//go:build integration

package config

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// Deterministic lock tests for UpdateServiceConfigAtomic (issue #1488). The
// handler-level race tests in internal/api fire real concurrent PUTs; these
// pin each of the two locks the store method takes by parking a competing
// writer mid-transaction and checking the update queues behind it and then
// merges onto what that writer committed.

// holdServiceConfigWriter opens a transaction for the test to play a competing
// writer in. It rolls back at test end unless the test committed it.
func holdServiceConfigWriter(ctx context.Context, t *testing.T, store *PostgresStore) pgx.Tx {
	t.Helper()
	tx, err := store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			t.Logf("writer rollback: %v", rbErr)
		}
	})
	return tx
}

// setCoverage80 is the update under test: a partial PUT of coverage only.
func setCoverage80(existing *ServiceConfig) (*ServiceConfig, error) {
	next := &ServiceConfig{Provider: "aws", Service: "rds", Term: 1, Payment: "no-upfront"}
	if existing != nil {
		copied := *existing
		next = &copied
	}
	next.Coverage = 80
	return next, nil
}

// TestUpdateServiceConfigAtomic_QueuesBehindRowWriter: the global-defaults
// propagation rewrites service rows with a plain UPDATE, without the
// per-service advisory lock. The FOR UPDATE read must wait for it and merge
// onto its committed term; reading the row unlocked would write the old term
// back over it.
func TestUpdateServiceConfigAtomic_QueuesBehindRowWriter(t *testing.T) {
	ctx := context.Background()
	store := NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB)
	require.NoError(t, store.SaveServiceConfig(ctx, &ServiceConfig{
		Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 50, RampSchedule: RampImmediate,
	}))

	writer := holdServiceConfigWriter(ctx, t, store)
	_, err := writer.Exec(ctx, `UPDATE service_configs SET term = 3 WHERE provider = 'aws' AND service = 'rds'`)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, uErr := store.UpdateServiceConfigAtomic(ctx, "aws", "rds", setCoverage80)
		done <- uErr
	}()
	testhelpers.WaitForLockWait(ctx, t, store.db, "%service_configs%")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	got, err := store.GetServiceConfig(ctx, "aws", "rds")
	require.NoError(t, err)
	require.Equal(t, 3, got.Term, "the concurrent writer's term must survive")
	require.Equal(t, 80.0, got.Coverage)
}

// TestUpdateServiceConfigAtomic_QueuesBehindFirstInsert: when no row exists
// there is nothing to lock FOR UPDATE, so only the advisory lock stops two
// first saves from both reading nothing. A competing save holding that lock
// inserts the row; the update must wait, then merge onto the inserted row
// rather than overwrite it with its own fresh one.
func TestUpdateServiceConfigAtomic_QueuesBehindFirstInsert(t *testing.T) {
	ctx := context.Background()
	store := NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB)

	writer := holdServiceConfigWriter(ctx, t, store)
	_, err := writer.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", serviceConfigLockPrefix+"aws/rds")
	require.NoError(t, err)
	require.NoError(t, saveServiceConfigWith(ctx, writer, &ServiceConfig{
		Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 50, RampSchedule: RampImmediate, MinCount: 5,
	}))

	done := make(chan error, 1)
	go func() {
		_, uErr := store.UpdateServiceConfigAtomic(ctx, "aws", "rds", setCoverage80)
		done <- uErr
	}()
	testhelpers.WaitForLockWait(ctx, t, store.db, "")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	got, err := store.GetServiceConfig(ctx, "aws", "rds")
	require.NoError(t, err)
	require.Equal(t, 5, got.MinCount, "the first save's min_count must survive")
	require.Equal(t, 80.0, got.Coverage)
	require.True(t, got.Enabled)
}
