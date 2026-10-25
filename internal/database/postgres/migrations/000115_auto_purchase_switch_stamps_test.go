//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_AutoPurchaseSwitchStamps pins migration 000115 against a real
// PostgreSQL: an existing global_config row keeps the kill switch OFF and gets
// NULL stamps, the up statement is idempotent, and the down migration drops
// the three columns.
func TestMigration_AutoPurchaseSwitchStamps(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 115)))
	_, err := pool.Exec(ctx, `INSERT INTO global_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 115))
	var enabled bool
	var by, via *string
	var hasAt bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT auto_purchase_enabled, auto_purchase_changed_by, auto_purchase_changed_at IS NOT NULL, auto_purchase_changed_via
		FROM global_config WHERE id = 1`).Scan(&enabled, &by, &hasAt, &via))
	assert.False(t, enabled, "the kill switch stays OFF across the migration")
	assert.Nil(t, by)
	assert.False(t, hasAt)
	assert.Nil(t, via)

	up, err := os.ReadFile(filepath.Join(migrationsPath, "000115_auto_purchase_switch_stamps.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 114))
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'global_config' AND column_name LIKE 'auto_purchase_changed_%'`).Scan(&cols))
	assert.Equal(t, 0, cols, "down migration must drop the columns")
}
