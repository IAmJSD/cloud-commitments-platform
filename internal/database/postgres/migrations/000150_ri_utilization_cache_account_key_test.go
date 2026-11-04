//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

const (
	m150AccountA = "15000000-0000-4000-8000-00000000000a"
	m150AccountB = "15000000-0000-4000-8000-00000000000b"
)

// m150PrimaryKey returns ri_utilization_cache's primary key columns in key
// order.
func m150PrimaryKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT a.attname
		  FROM pg_index i
		  JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE i.indrelid = 'ri_utilization_cache'::regclass AND i.indisprimary
		 ORDER BY k.ord`)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	return cols
}

func m150Count(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM ri_utilization_cache`).Scan(&n))
	return n
}

// TestMigration_RIUtilizationCacheAccountKey covers 000150 (issue #120): the
// cache key gains cloud_account_id, rows written without an account identity
// are dropped, two accounts can hold rows for the same region and lookback,
// a write must name its account, and the down migration restores the
// (region, lookback_days) key.
func TestMigration_RIUtilizationCacheAccountKey(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	const version = 150
	prev := previousMigrationVersion(t, version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Equal(t, []string{"region", "lookback_days"}, m150PrimaryKey(ctx, t, pool))

	// A pre-000150 row carries no account identity.
	_, err := pool.Exec(ctx, `
		INSERT INTO ri_utilization_cache (region, lookback_days, payload, fetched_at)
		VALUES ('us-east-1', 30, '[{"ReservedInstanceID":"ri-legacy"}]', NOW())`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	assert.Equal(t, []string{"cloud_account_id", "region", "lookback_days"}, m150PrimaryKey(ctx, t, pool))
	assert.Zero(t, m150Count(ctx, t, pool), "rows without an account identity are dropped, not attributed to one")

	insert := func(account string) error {
		_, insertErr := pool.Exec(ctx, `
			INSERT INTO ri_utilization_cache (cloud_account_id, region, lookback_days, payload, fetched_at)
			VALUES ($1, 'us-east-1', 30, '[]', NOW())`, account)
		return insertErr
	}
	require.NoError(t, insert(m150AccountA))
	require.NoError(t, insert(m150AccountB), "a second account can cache the same region and lookback")
	require.NoError(t, insert(""), "the unregistered ambient identity has its own key")
	assert.Error(t, insert(m150AccountA), "one account still has one row per region and lookback")

	_, err = pool.Exec(ctx, `
		INSERT INTO ri_utilization_cache (region, lookback_days, payload, fetched_at)
		VALUES ('us-west-2', 30, '[]', NOW())`)
	assert.Error(t, err, "a write that does not name its account is refused")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, []string{"region", "lookback_days"}, m150PrimaryKey(ctx, t, pool), "down restores the old key")
	assert.Zero(t, m150Count(ctx, t, pool), "down empties the cache so per-account rows cannot collide on the old key")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	assert.Equal(t, []string{"cloud_account_id", "region", "lookback_days"}, m150PrimaryKey(ctx, t, pool), "re-up")
}
