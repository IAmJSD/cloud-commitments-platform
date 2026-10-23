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

// TestMigration_000146_ClearRevokedInFlight covers the data half of issue #316
// (A04-013): before the fix MarkPurchaseRevoked left revocation_in_flight set,
// so every completed Azure revocation still reads back as in flight. 000146
// clears the flag on revoked rows only; an unrevoked flagged row is the
// finalize_revocations sweep's work queue and must keep it.
func TestMigration_000146_ClearRevokedInFlight(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	pool := testhelpers.RequireMigratedDatabase(ctx, t, path).DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 146)))

	revokedAt := time.Now().UTC()
	rows := []struct {
		id        string
		inFlight  bool
		revokedAt *time.Time
		want      bool
	}{
		{"ri-146-revoked-flagged", true, &revokedAt, false}, // the stale flag
		{"ri-146-pending-flagged", true, nil, true},         // sweep must still see it
		{"ri-146-revoked-clear", false, &revokedAt, false},  // untouched
		{"ri-146-active", false, nil, false},                // untouched
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

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 146))

	for _, r := range rows {
		var got bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT revocation_in_flight FROM purchase_history WHERE purchase_id = $1`, r.id).Scan(&got))
		assert.Equal(t, r.want, got, r.id)
	}

	// The down is a deliberate no-op: it must apply and leave the rows alone.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 146)))
	var flagged int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM purchase_history WHERE revocation_in_flight AND purchase_id LIKE 'ri-146-%'`).Scan(&flagged))
	assert.Equal(t, 1, flagged, "only the unrevoked in-flight row stays flagged after the rollback")
}
