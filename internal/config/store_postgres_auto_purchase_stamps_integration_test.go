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
