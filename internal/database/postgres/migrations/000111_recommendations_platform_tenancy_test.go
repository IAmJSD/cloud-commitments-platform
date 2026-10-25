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

const insertRecForMigration111 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES (NOW(), 'aws', 'ec2', 'us-east-1', $1, '', $2::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_RecommendationsPlatformTenancy locks down migration 000111
// (issue #806, PR-A): the generated platform and tenancy columns are read
// verbatim from payload->'details', the 9-column index is kept so the current
// upsert still works, the 11-column index is added, and rollback removes only
// the new column pair and index.
func TestMigration_RecommendationsPlatformTenancy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 110))

	// Rows as written before the migration. The vectors cover every payload
	// shape; the Go helper added in PR-B must agree with them.
	type vector struct{ payload, platform, tenancy string }
	vectors := map[string]vector{
		"details-absent":  {`{"service":"ec2"}`, "", ""},
		"details-null":    {`{"details":null}`, "", ""},
		"details-empty":   {`{"details":{}}`, "", ""},
		"platform-null":   {`{"details":{"platform":null,"tenancy":null}}`, "", ""},
		"aws-linux":       {`{"details":{"platform":"Linux/UNIX","tenancy":"default"}}`, "Linux/UNIX", "default"},
		"aws-rhel":        {`{"details":{"platform":"Red Hat Enterprise Linux","tenancy":"dedicated"}}`, "Red Hat Enterprise Linux", "dedicated"},
		"verbatim-spaces": {`{"details":{"platform":" x ","tenancy":" Y "}}`, " x ", " Y "},
		"empty-strings":   {`{"details":{"platform":"","tenancy":""}}`, "", ""},
		// A top-level key is NOT consulted: only Details carries these fields.
		"top-level-ignored": {`{"platform":"Windows","tenancy":"host"}`, "", ""},
	}
	for resourceType, v := range vectors {
		_, err := pool.Exec(ctx, insertRecForMigration111, resourceType, v.payload)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 111))

	rows, err := pool.Query(ctx, `SELECT resource_type, platform, tenancy FROM recommendations`)
	require.NoError(t, err)
	got := map[string]vector{}
	for rows.Next() {
		var rt string
		var v vector
		require.NoError(t, rows.Scan(&rt, &v.platform, &v.tenancy))
		got[rt] = v
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, len(vectors))
	for rt, want := range vectors {
		assert.Equal(t, want.platform, got[rt].platform, "platform of %s", rt)
		assert.Equal(t, want.tenancy, got[rt].tenancy, "tenancy of %s", rt)
	}

	// Both indexes exist: the 9-column one is kept.
	for _, idx := range []string{"recommendations_natural_key_az_idx", "recommendations_natural_key_az_tp_idx"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'recommendations' AND indexname = $1)`, idx).Scan(&exists))
		assert.True(t, exists, "index %s must exist after 000111", idx)
	}
	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_tp_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "payment_option, availability_zone, platform, tenancy)")

	// Old code's upsert (9-column ON CONFLICT, no platform/tenancy) still works
	// and updates the existing row in place.
	upsert := `
		INSERT INTO recommendations
		    (collected_at, provider, service, region, resource_type, engine, payload,
		     upfront_cost, monthly_savings, term, payment_option)
		VALUES (NOW(), 'aws', 'ec2', 'us-east-1', 'aws-linux', '', $1::jsonb, 5, 0, 1, 'all-upfront')
		ON CONFLICT (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone)
		DO UPDATE SET payload = EXCLUDED.payload, upfront_cost = EXCLUDED.upfront_cost`
	_, err = pool.Exec(ctx, upsert, `{"details":{"platform":"Linux/UNIX","tenancy":"default"}}`)
	require.NoError(t, err, "the existing 9-column upsert must keep working after 000111")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations WHERE resource_type = 'aws-linux'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Known limitation, resolved in PR-E: two rows differing only by platform
	// (or tenancy) are still rejected by the kept 9-column index.
	for _, pair := range [][2]string{
		{`{"details":{"platform":"Linux/UNIX","tenancy":"default"}}`, `{"details":{"platform":"Windows","tenancy":"default"}}`},
		{`{"details":{"platform":"Linux/UNIX","tenancy":"default"}}`, `{"details":{"platform":"Linux/UNIX","tenancy":"dedicated"}}`},
	} {
		rt := "pair-" + pair[1]
		_, err = pool.Exec(ctx, insertRecForMigration111, rt, pair[0])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insertRecForMigration111, rt, pair[1])
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23505", pgErr.Code)
		assert.Equal(t, "recommendations_natural_key_az_idx", pgErr.ConstraintName)
	}

	// Rollback removes the new columns and index only.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 110))
	for _, col := range []string{"platform", "tenancy"} {
		var colExists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'recommendations' AND column_name = $1)`, col).Scan(&colExists))
		assert.False(t, colExists, "column %s must be gone after rollback", col)
	}
	var newIdx, oldIdx bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_tp_idx')`).Scan(&newIdx))
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_idx')`).Scan(&oldIdx))
	assert.False(t, newIdx)
	assert.True(t, oldIdx)
}
