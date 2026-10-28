//go:build integration
// +build integration

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

// TestMigration_000148_RevocationReturnPosted covers the schema half of issue
// #1523. Rows flagged in flight before 000148 have no record of an accepted
// Return, so the up must not invent one (a fabricated return_posted_at would
// let the sweep mark a possibly unrefunded purchase revoked) and must not
// touch the flag or revoked_at. The down drops both columns and leaves the
// flag alone.
func TestMigration_000148_RevocationReturnPosted(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	pool := testhelpers.RequireMigratedDatabase(ctx, t, path).DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 148)))

	revokedAt := time.Now().UTC()
	rows := []struct {
		id        string
		inFlight  bool
		revokedAt *time.Time
	}{
		{"ri-148-flagged", true, nil},
		{"ri-148-revoked", false, &revokedAt},
		{"ri-148-active", false, nil},
	}
	for _, r := range rows {
		_, err := pool.Exec(ctx, `
			INSERT INTO purchase_history (
				account_id, purchase_id, timestamp, provider, service, region,
				resource_type, term, payment, revocation_in_flight, revoked_at
			) VALUES ('acct', $1, NOW(), 'azure', 'compute', 'eastus', 'Standard_D2s_v3', 12, 'All Upfront', $2, $3)`,
			r.id, r.inFlight, r.revokedAt)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 148))

	for _, r := range rows {
		var inFlight bool
		var revoked, returnPosted, inFlightAt *time.Time
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT revocation_in_flight, revoked_at, return_posted_at, revocation_in_flight_at
			  FROM purchase_history WHERE purchase_id = $1`, r.id).Scan(&inFlight, &revoked, &returnPosted, &inFlightAt))
		assert.Equal(t, r.inFlight, inFlight, "%s: the flag is untouched", r.id)
		assert.Equal(t, r.revokedAt != nil, revoked != nil, "%s: revoked_at is untouched", r.id)
		assert.Nil(t, returnPosted, "%s: no Return is recorded for a pre-000148 row", r.id)
		assert.Nil(t, inFlightAt, "%s: the sweep, not the migration, starts a legacy flag's clock", r.id)
	}
	var indexed int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_purchase_history_return_posted_unrevoked'`).Scan(&indexed))
	assert.Equal(t, 1, indexed)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 148)))
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'purchase_history'
		   AND column_name IN ('return_posted_at', 'revocation_in_flight_at')`).Scan(&cols))
	assert.Zero(t, cols, "the down drops both columns")
	var flagged int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM purchase_history WHERE revocation_in_flight AND purchase_id LIKE 'ri-148-%'`).Scan(&flagged))
	assert.Equal(t, 1, flagged, "the down leaves the flag alone")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 148), "the up re-applies after a rollback")
}
