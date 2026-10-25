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

// TestMigration_LadderTranchesStatusReason pins migration 000114 against a real
// PostgreSQL: a pre-existing tranche gets a NULL reason, the up statement is
// idempotent, and the down migration drops the column.
func TestMigration_LadderTranchesStatusReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 114)))
	var trancheID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO ladder_tranches (layer_type, term, payment_option, status, amount_usd_hr, scheduled_date)
		VALUES ('compute-sp', '1y', 'no_upfront', 'scheduled', 1, NOW())
		RETURNING id`).Scan(&trancheID))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 114))
	var reason *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status_reason FROM ladder_tranches WHERE id = $1`, trancheID).Scan(&reason))
	assert.Nil(t, reason, "an existing tranche has no reason")

	up, err := os.ReadFile(filepath.Join(migrationsPath, "000114_ladder_tranches_status_reason.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 113))
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'ladder_tranches' AND column_name = 'status_reason'`).Scan(&cols))
	assert.Equal(t, 0, cols, "down migration must drop the column")
}
