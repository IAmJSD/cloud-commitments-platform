//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_PlanStepCapAndAutoPurchaseSwitch pins migration 000112 against
// a real PostgreSQL: the new columns are additive, a pre-existing plan comes
// out with a NULL cap (fail closed: it cannot auto-purchase), the global kill
// switch defaults to FALSE, the CHECK rejects a non-positive cap, and the down
// migration removes both columns.
func TestMigration_PlanStepCapAndAutoPurchaseSwitch(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 112)))

	var planID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_plans (name, enabled, auto_purchase, notification_days_before, services, ramp_schedule)
		VALUES ('pre-existing', true, true, 3, '{}'::jsonb, '{}'::jsonb)
		RETURNING id`).Scan(&planID))
	_, err := pool.Exec(ctx, `INSERT INTO global_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 112))

	var cap *float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT max_step_commitment FROM purchase_plans WHERE id = $1`, planID).Scan(&cap))
	assert.Nil(t, cap, "an existing plan must get a NULL cap so it stays unable to auto-purchase")

	var enabled bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT auto_purchase_enabled FROM global_config WHERE id = 1`).Scan(&enabled))
	assert.False(t, enabled, "the kill switch must default to off")

	for _, bad := range []string{"0", "-5"} {
		_, err = pool.Exec(ctx, `UPDATE purchase_plans SET max_step_commitment = `+bad+` WHERE id = $1`, planID)
		require.Error(t, err, "a non-positive cap %s must be rejected by the CHECK", bad)
	}
	_, err = pool.Exec(ctx, `UPDATE purchase_plans SET max_step_commitment = 1234.56 WHERE id = $1`, planID)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 111))
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'purchase_plans' AND column_name = 'max_step_commitment')
		   OR (table_name = 'global_config' AND column_name = 'auto_purchase_enabled')`).Scan(&cols))
	assert.Equal(t, 0, cols, "down migration must drop both columns")
}
