//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration_LadderTrancheAmendments(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 106))

	var hasRevision bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'ladder_tranches' AND column_name = 'revision')`).Scan(&hasRevision))
	assert.False(t, hasRevision, "revision column must not exist before 000107")

	var trancheID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ladder_tranches
		(layer_type, amount_usd_hr, term, payment_option, scheduled_date, status)
		VALUES ('ec2-instance-sp', 1.250000, '1yr', 'no-upfront', NOW() + INTERVAL '1 day', 'scheduled')
		RETURNING id`).Scan(&trancheID))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 107))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 107), "re-targeting 107 must be a no-op")

	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'ladder_tranches' AND column_name = 'revision')`).Scan(&hasRevision))
	assert.True(t, hasRevision)

	var revision int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM ladder_tranches WHERE id = $1`, trancheID).Scan(&revision))
	assert.Equal(t, int64(0), revision, "pre-existing tranches start at revision 0")

	_, err := pool.Exec(ctx, `UPDATE ladder_tranches SET revision = -1 WHERE id = $1`, trancheID)
	require.Error(t, err, "CHECK (revision >= 0) must reject negative revisions")

	_, err = pool.Exec(ctx, `INSERT INTO ladder_tranche_amendments
		(tranche_id, actor, previous_revision, new_revision,
		 previous_amount_usd_hr, new_amount_usd_hr, previous_scheduled_date, new_scheduled_date)
		VALUES ($1, 'tester@example.com', 0, 1, 1.250000, 2.500000, NOW(), NOW() + INTERVAL '2 days')`, trancheID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO ladder_tranche_amendments
		(tranche_id, actor, previous_revision, new_revision,
		 previous_amount_usd_hr, new_amount_usd_hr, previous_scheduled_date, new_scheduled_date)
		VALUES ($1, 'tester@example.com', 1, 3, 2.500000, 3.000000, NOW(), NOW() + INTERVAL '3 days')`, trancheID)
	require.Error(t, err, "CHECK (new_revision = previous_revision + 1) must reject revision gaps")

	_, err = pool.Exec(ctx, `INSERT INTO ladder_tranche_amendments
		(tranche_id, actor, previous_revision, new_revision,
		 previous_amount_usd_hr, new_amount_usd_hr, previous_scheduled_date, new_scheduled_date)
		VALUES ($1, 'tester@example.com', 0, 1, 1.250000, 1.500000, NOW(), NOW() + INTERVAL '4 days')`, trancheID)
	require.Error(t, err, "UNIQUE (tranche_id, new_revision) must reject duplicate revisions")

	_, err = pool.Exec(ctx, `INSERT INTO ladder_tranche_amendments
		(tranche_id, actor, previous_revision, new_revision,
		 previous_amount_usd_hr, new_amount_usd_hr, previous_scheduled_date, new_scheduled_date)
		VALUES ('00000000-0000-0000-0000-000000000000', 'tester@example.com', 0, 1,
		 1.000000, 2.000000, NOW(), NOW() + INTERVAL '2 days')`)
	require.Error(t, err, "amendments must reference an existing tranche")

	err = migrations.MigrateToVersion(ctx, pool, path, 106)
	require.Error(t, err, "000107 rollback must refuse to destroy audit history")
	assert.Contains(t, err.Error(), "amendment history")

	var auditRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM ladder_tranche_amendments WHERE tranche_id = $1`, trancheID).Scan(&auditRows))
	assert.Equal(t, 1, auditRows, "audit rows must survive the refused rollback")

	var hasTable bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'ladder_tranche_amendments')`).Scan(&hasTable))
	assert.True(t, hasTable, "amendment table must survive the refused rollback")
}

func TestMigration_LadderTrancheAmendmentsRollbackWhenEmpty(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 107))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 106), "empty amendment history must not block rollback")

	var leftovers int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name = 'ladder_tranches' AND column_name = 'revision')`).Scan(&leftovers))
	assert.Zero(t, leftovers)
	var hasTable bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'ladder_tranche_amendments')`).Scan(&hasTable))
	assert.False(t, hasTable)
}
