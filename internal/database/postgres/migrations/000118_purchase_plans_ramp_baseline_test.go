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

// TestMigration_PurchasePlansRampBaseline pins migration 000118 against a real
// PostgreSQL: existing plans get a NULL baseline (no backfill), the up
// statement is idempotent, and down removes the column.
func TestMigration_PurchasePlansRampBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 118)))
	var planID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO purchase_plans (name) VALUES ('legacy') RETURNING id`).Scan(&planID))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 118))
	var isNull bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT ramp_baseline IS NULL FROM purchase_plans WHERE id = $1`, planID).Scan(&isNull))
	assert.True(t, isNull, "a plan that predates the migration has no baseline")

	up, err := os.ReadFile(filepath.Join(path, "000118_purchase_plans_ramp_baseline.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "up must be idempotent")

	down, err := os.ReadFile(filepath.Join(path, "000118_purchase_plans_ramp_baseline.down.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err)
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'purchase_plans' AND column_name = 'ramp_baseline'`).Scan(&cols))
	assert.Zero(t, cols)
}
