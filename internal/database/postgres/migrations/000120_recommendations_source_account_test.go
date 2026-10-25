//go:build integration

package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration_RecommendationsSourceAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	path := getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 120)))
	const insert = `INSERT INTO recommendations
		(collected_at, provider, service, region, resource_type, engine, payload, upfront_cost, monthly_savings, term, payment_option)
		VALUES (NOW(), 'aws', 'ec2', 'us-east-1', $1, '', $2::jsonb, 0, 0, 1, 'all-upfront')`
	vectors := map[string]struct{ payload, source string }{
		"absent":   {`{}`, ""},
		"null":     {`{"source_account_id":null}`, ""},
		"empty":    {`{"source_account_id":""}`, ""},
		"known":    {`{"source_account_id":"SourceAbC"}`, "SourceAbC"},
		"verbatim": {`{"source_account_id":" A|β\t "}`, " A|β\t "},
	}
	for name, v := range vectors {
		_, err := pool.Exec(ctx, insert, name, v.payload)
		require.NoError(t, err)
	}
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 120))
	for name, v := range vectors {
		var source, payload string
		require.NoError(t, pool.QueryRow(ctx, `SELECT source_account_key, payload::text FROM recommendations WHERE resource_type=$1`, name).Scan(&source, &payload))
		assert.Equal(t, v.source, source)
		assert.JSONEq(t, v.payload, payload)
	}
	const oldIndex = "recommendations_natural_key_az_tp_idx"
	const newIndex = "recommendations_natural_key_source_idx"
	for _, index := range []string{oldIndex, newIndex} {
		var definition string
		require.NoError(t, pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname=$1`, index).Scan(&definition))
		assert.Contains(t, definition, "UNIQUE INDEX")
		if index == newIndex {
			assert.Contains(t, definition, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, source_account_key)")
		}
	}
	_, err := pool.Exec(ctx, insert, "known", `{"source_account_id":"OtherSource"}`)
	var constraint *pgconn.PgError
	require.ErrorAs(t, err, &constraint)
	assert.Equal(t, "23505", constraint.Code)
	assert.Equal(t, oldIndex, constraint.ConstraintName)
	updated := vectors["known"]
	updated.payload = `{"source_account_id":"SourceAbC","proof":"updated"}`
	vectors["known"] = updated
	_, err = pool.Exec(ctx, insert+` ON CONFLICT (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy)
		DO UPDATE SET payload=EXCLUDED.payload`, "known", vectors["known"].payload)
	require.NoError(t, err)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations`).Scan(&count))
	assert.Equal(t, len(vectors), count)

	down, err := os.ReadFile(filepath.Join(path, "000120_recommendations_source_account.down.sql"))
	require.NoError(t, err)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `DROP INDEX recommendations_natural_key_az_tp_idx`)
	require.NoError(t, err)
	_, guardErr := tx.Exec(ctx, string(down))
	require.NoError(t, tx.Rollback(ctx))
	require.ErrorContains(t, guardErr, "cannot remove source account key without legacy unique index")
	var preserved bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname=$1)`, newIndex).Scan(&preserved))
	assert.True(t, preserved)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 120)))
	for name, v := range vectors {
		var payload string
		require.NoError(t, pool.QueryRow(ctx, `SELECT payload::text FROM recommendations WHERE resource_type=$1`, name).Scan(&payload))
		assert.JSONEq(t, v.payload, payload)
	}
	for _, query := range []string{
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname='recommendations_natural_key_source_idx')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='recommendations' AND column_name='source_account_key')`,
	} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, query).Scan(&exists))
		assert.False(t, exists)
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname=$1)`, oldIndex).Scan(&preserved))
	assert.True(t, preserved)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 120))
}
