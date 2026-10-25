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

// Migration 000104 adds users.mfa_last_totp_counter. Existing rows start at 0
// (no code accepted yet), and the down migration removes the column.
func TestMigration_UserMFALastTOTPCounter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 104)))
	userID := seedDeactivatedAtUser(ctx, t, pool, "totp-counter@example.com", true, nil, nil)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 104))
	var counter int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT mfa_last_totp_counter FROM users WHERE id = $1`, userID).Scan(&counter))
	assert.Zero(t, counter, "an existing user must start with no accepted code")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 104)))
	err := pool.QueryRow(ctx, `SELECT mfa_last_totp_counter FROM users WHERE id = $1`, userID).Scan(&counter)
	require.Error(t, err, "the down migration must drop the column")
}
