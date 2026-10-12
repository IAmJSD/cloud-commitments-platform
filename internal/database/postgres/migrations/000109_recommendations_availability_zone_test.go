//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const insertRecForMigration109 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES (NOW(), 'aws', 'ec2', 'us-east-1', $1, '', $2::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_RecommendationsAvailabilityZone locks down migration 000109
// (issue #792, PR2a): the generated availability_zone column backfills from
// both payload shapes, the 8-column index is kept so the current upsert still
// works, and rollback removes the column and the new index.
func TestMigration_RecommendationsAvailabilityZone(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 108))

	// Rows as written before the migration, in every payload shape.
	legacy := map[string]string{
		// Written before the top-level field existed: AZ only inside Details.
		"legacy-details-only": `{"details":{"instance_type":"m5.large","scope":"Availability Zone","availability_zone":"us-east-1a"}}`,
		// Written by the scheduler since #807: top-level and Details agree.
		"both-shapes": `{"availability_zone":"us-east-1b","details":{"instance_type":"m5.large","scope":"Availability Zone","availability_zone":"us-east-1b"}}`,
		// Top-level only.
		"top-level-only": `{"availability_zone":"us-east-1c"}`,
		// Regional EC2 rec: Details present, no AZ.
		"regional": `{"details":{"instance_type":"m5.large","scope":"Region"}}`,
		// No details at all.
		"no-details": `{"service":"ec2"}`,
	}
	for resourceType, payload := range legacy {
		_, err := pool.Exec(ctx, insertRecForMigration109, resourceType, payload)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 109))

	want := map[string]string{
		"legacy-details-only": "us-east-1a",
		"both-shapes":         "us-east-1b",
		"top-level-only":      "us-east-1c",
		"regional":            "",
		"no-details":          "",
	}
	rows, err := pool.Query(ctx, `SELECT resource_type, availability_zone FROM recommendations`)
	require.NoError(t, err)
	got := map[string]string{}
	for rows.Next() {
		var rt, az string
		require.NoError(t, rows.Scan(&rt, &az))
		got[rt] = az
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got, "availability_zone must be backfilled from either payload shape and default to ''")

	// Both indexes exist: the 8-column one is kept.
	for _, idx := range []string{"recommendations_natural_key_idx", "recommendations_natural_key_az_idx"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'recommendations' AND indexname = $1)`, idx).Scan(&exists))
		assert.True(t, exists, "index %s must exist after 000109", idx)
	}

	// The 9-column index is unique and ends with availability_zone. It cannot be
	// exercised by inserts yet (the kept 8-column index rejects the same rows),
	// so assert its definition.
	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "engine, term, payment_option, availability_zone)")

	// Old code's upsert (8-column ON CONFLICT, no availability_zone) still works
	// and updates the existing row in place.
	upsert := `
		INSERT INTO recommendations
		    (collected_at, provider, service, region, resource_type, engine, payload,
		     upfront_cost, monthly_savings, term, payment_option)
		VALUES (NOW(), 'aws', 'ec2', 'us-east-1', 'both-shapes', '', $1::jsonb, 5, 0, 1, 'all-upfront')
		ON CONFLICT (account_key, provider, service, region, resource_type, engine, term, payment_option)
		DO UPDATE SET payload = EXCLUDED.payload, upfront_cost = EXCLUDED.upfront_cost`
	_, err = pool.Exec(ctx, upsert, `{"availability_zone":"us-east-1b"}`)
	require.NoError(t, err, "the existing 8-column upsert must keep working after 000109")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations WHERE resource_type = 'both-shapes'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Known limitation, resolved in PR2c: two zonal rows differing only by AZ
	// are still rejected by the kept 8-column index.
	_, err = pool.Exec(ctx, insertRecForMigration109, "zonal-pair", `{"availability_zone":"us-east-1a"}`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration109, "zonal-pair", `{"availability_zone":"us-east-1b"}`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "recommendations_natural_key_idx", pgErr.ConstraintName)

	// Rollback removes the column and the new index only.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 108))
	var colExists, newIdx, oldIdx bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'recommendations' AND column_name = 'availability_zone')`).Scan(&colExists))
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_idx')`).Scan(&newIdx))
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_idx')`).Scan(&oldIdx))
	assert.False(t, colExists)
	assert.False(t, newIdx)
	assert.True(t, oldIdx)
}
