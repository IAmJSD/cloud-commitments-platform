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

const insertRecForMigration152 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES (NOW(), 'aws', 'rds', 'us-east-1', $1, 'mysql', $2::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_RecommendationsAZConfig locks down migration 000152 (issue
// #408, expand step): the generated az_config column is read verbatim from
// payload->'details', the 11-column index is kept so the previous upsert still
// works, the 12-column index is added, and rollback removes only the new
// column and index. The vectors are mirrored by TestDetailsAZConfig_Vectors
// (internal/config) so the Go reader cannot diverge from the column.
func TestMigration_RecommendationsAZConfig(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 152)))

	type vector struct{ payload, azConfig string }
	vectors := map[string]vector{
		"details-absent":       {`{"service":"rds"}`, ""},
		"details-null":         {`{"details":null}`, ""},
		"details-empty":        {`{"details":{}}`, ""},
		"az-config-null":       {`{"details":{"az_config":null}}`, ""},
		"empty-string":         {`{"details":{"az_config":""}}`, ""},
		"rds-single-az":        {`{"details":{"az_config":"single-az"}}`, "single-az"},
		"rds-multi-az":         {`{"details":{"az_config":"multi-az"}}`, "multi-az"},
		"azure-zone-redundant": {`{"details":{"az_config":"zoneRedundant"}}`, "zoneRedundant"},
		"verbatim-spaces":      {`{"details":{"az_config":" Multi-AZ "}}`, " Multi-AZ "},
		"compute-details":      {`{"details":{"platform":"Linux/UNIX","tenancy":"default"}}`, ""},
		// A top-level key is NOT consulted: only Details carries the field.
		"top-level-ignored": {`{"az_config":"multi-az"}`, ""},
	}
	for name, v := range vectors {
		_, err = pool.Exec(ctx, insertRecForMigration152, name, v.payload)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 152))

	for name, v := range vectors {
		var got string
		require.NoError(t, pool.QueryRow(ctx, `SELECT az_config FROM recommendations WHERE resource_type = $1`, name).Scan(&got))
		assert.Equal(t, v.azConfig, got, "az_config of %s", name)
	}

	for _, idx := range []string{
		"recommendations_natural_key_az_tp_idx", "recommendations_natural_key_az_tp_azc_idx",
		"recommendations_natural_key_source_idx", "recommendations_natural_key_source_azc_idx",
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'recommendations' AND indexname = $1)`, idx).Scan(&exists))
		assert.True(t, exists, "index %s must exist after 000152", idx)
	}
	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_tp_azc_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, az_config)")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_source_azc_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, az_config, source_account_key)")

	// The previous code's upsert (11-column ON CONFLICT) still works and
	// updates the existing row in place, so 000152 can be applied before the
	// store switch is released.
	singleAZ := `{"details":{"az_config":"single-az"}}`
	_, err = pool.Exec(ctx, insertRecForMigration152+`
		ON CONFLICT (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy)
		DO UPDATE SET upfront_cost = 5`, "pair", singleAZ)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration152+`
		ON CONFLICT (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy)
		DO UPDATE SET upfront_cost = 5`, "pair", singleAZ)
	require.NoError(t, err, "the existing 11-column upsert must keep working after 000152")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations WHERE resource_type = 'pair'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Known limitation, resolved by 000153: a row differing only by az_config
	// is still rejected by the kept indexes without az_config.
	_, err = pool.Exec(ctx, insertRecForMigration152, "pair", `{"details":{"az_config":"multi-az"}}`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Contains(t, []string{"recommendations_natural_key_az_tp_idx", "recommendations_natural_key_source_idx"}, pgErr.ConstraintName)

	// Rollback removes the new column and index only.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 152)))
	var colExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'recommendations' AND column_name = 'az_config')`).Scan(&colExists))
	assert.False(t, colExists)
	for idx, want := range map[string]bool{
		"recommendations_natural_key_az_tp_azc_idx":  false,
		"recommendations_natural_key_source_azc_idx": false,
		"recommendations_natural_key_az_tp_idx":      true,
		"recommendations_natural_key_source_idx":     true,
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx).Scan(&exists))
		assert.Equal(t, want, exists, "index %s after rollback", idx)
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations`).Scan(&n))
	assert.Equal(t, len(vectors)+1, n, "rollback must not delete rows")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 152))
}
