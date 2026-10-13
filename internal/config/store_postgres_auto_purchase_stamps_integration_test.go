//go:build integration
// +build integration

package config

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgresStoreDB_AutoPurchaseStamps_FreshMigrateAndRIWritePreserve runs
// against a dedicated database named r1a_* (AUTO_PURCHASE_STAMPS_TEST_DATABASE_URL):
// a fresh migrate leaves the kill switch OFF with NULL stamps, and the RI-exchange
// style whole-config read-modify-write keeps the stamps instead of wiping them.
func TestPostgresStoreDB_AutoPurchaseStamps_FreshMigrateAndRIWritePreserve(t *testing.T) {
	dsn := os.Getenv("AUTO_PURCHASE_STAMPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set AUTO_PURCHASE_STAMPS_TEST_DATABASE_URL to a dedicated r1a_* test database")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "r1a_"), "database must be dedicated to R1a verification")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getTestMigrationsPath(), "", ""))
	store := &PostgresStore{db: pool}

	cfg, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	assert.False(t, cfg.AutoPurchaseEnabled, "a fresh migrate leaves the kill switch OFF")
	assert.Nil(t, cfg.AutoPurchaseChangedBy)
	assert.Nil(t, cfg.AutoPurchaseChangedAt)
	assert.Nil(t, cfg.AutoPurchaseChangedVia)

	// R1b will write these; seed them directly, as nothing sets them yet.
	_, err = pool.Exec(ctx, `
		UPDATE global_config
		SET auto_purchase_enabled = TRUE, auto_purchase_changed_by = 'user-1',
		    auto_purchase_changed_at = '2026-10-13T01:02:03Z', auto_purchase_changed_via = 'user-session'
		WHERE id = 1`)
	require.NoError(t, err)

	// The RI-exchange write path: read the whole config, change RI fields, upsert.
	after, err := store.UpdateGlobalConfigAtomic(ctx, func(existing *GlobalConfig) error {
		existing.RIExchangeEnabled = true
		existing.RIExchangeMode = "manual"
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, after.AutoPurchaseChangedBy)

	cfg, err = store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	assert.True(t, cfg.RIExchangeEnabled)
	assert.True(t, cfg.AutoPurchaseEnabled)
	require.NotNil(t, cfg.AutoPurchaseChangedBy)
	assert.Equal(t, "user-1", *cfg.AutoPurchaseChangedBy)
	require.NotNil(t, cfg.AutoPurchaseChangedAt)
	assert.True(t, cfg.AutoPurchaseChangedAt.Equal(time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)))
	require.NotNil(t, cfg.AutoPurchaseChangedVia)
	assert.Equal(t, "user-session", *cfg.AutoPurchaseChangedVia)
}

// TestPostgresStoreDB_StepCapStoredEqualsCompared proves, on a real PostgreSQL,
// that for every cap StepCapCents accepts, the NUMERIC(18,2) column stores the
// same cents the gate compared (B1: 1.005 used to store as 1.01 while the gate
// saw 1.00), and that caps it rejects include the half-cent inputs.
func TestPostgresStoreDB_StepCapStoredEqualsCompared(t *testing.T) {
	dsn := os.Getenv("AUTO_PURCHASE_STAMPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set AUTO_PURCHASE_STAMPS_TEST_DATABASE_URL to a dedicated r1a_* test database")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "r1a_"), "database must be dedicated to R1a verification")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getTestMigrationsPath(), "", ""))
	store := &PostgresStore{db: pool}

	plan := &PurchasePlan{
		Name: "cap-roundtrip", Enabled: true, Services: map[string]ServiceConfig{},
		RampSchedule: RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1},
	}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))

	caps := []float64{100.10, 12345678.9, 0.07, 1e15, 999999999999999.99}
	for c := 1; c <= 3000; c++ {
		caps = append(caps, float64(c)/100)
	}
	for _, v := range caps {
		sent, err := StepCapCents(v)
		require.NoError(t, err, "cap %v", v)
		got, err := store.GetPurchasePlan(ctx, plan.ID)
		require.NoError(t, err)
		v := v
		got.MaxStepCommitment = &v
		require.NoError(t, store.UpdatePurchasePlan(ctx, got), "cap %v", v)
		var cents int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT (max_step_commitment * 100)::bigint FROM purchase_plans WHERE id = $1`, plan.ID).Scan(&cents))
		require.Equal(t, sent, cents, "stored cents must equal compared cents for cap %v", v)
	}

	for _, v := range []float64{1.005, 0.015, 1.0049, 1e-3, 1000.005} {
		_, err := StepCapCents(v)
		require.Error(t, err, "cap %v must be rejected, not rounded", v)
	}
}
