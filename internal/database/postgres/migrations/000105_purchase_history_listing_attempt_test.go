//go:build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000105 adds the nullable listing attempt columns to
// purchase_history: existing rows have no unresolved attempt, and the down
// migration removes both columns.
func TestMigration_PurchaseHistoryListingAttempt(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	const insertSQL = `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, term, payment
		) VALUES ('acct', 'ri-105', $1, 'aws', 'ec2', 'us-east-1', 't3.micro', 12, 'All Upfront')`

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 105)))
	_, err := pool.Exec(ctx, insertSQL, time.Now())
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 105))
	var token, schedule *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT listing_client_token, listing_price_schedule::text FROM purchase_history WHERE purchase_id = 'ri-105'`).
		Scan(&token, &schedule))
	assert.Nil(t, token, "an existing row has no unresolved attempt")
	assert.Nil(t, schedule)

	_, err = pool.Exec(ctx,
		`UPDATE purchase_history SET listing_client_token = 'tok', listing_price_schedule = '[{"term_months":12,"price":1}]' WHERE purchase_id = 'ri-105'`)
	require.NoError(t, err, "both columns must be writable")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 105)))
	err = pool.QueryRow(ctx, `SELECT listing_client_token FROM purchase_history WHERE purchase_id = 'ri-105'`).Scan(&token)
	require.Error(t, err, "the down migration must drop the column")
}
