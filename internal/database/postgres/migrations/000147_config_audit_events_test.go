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
	m147Account = "14700000-0000-4000-8000-000000000001"
	m147Import  = "14700000-0000-4000-8000-0000000000a1"
)

func m147Insert(ctx context.Context, pool *pgxpool.Pool, id, eventType string, parent, account, before, after any, actor string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO config_audit_events (id, event_type, parent_event_id, actor, account_id, provider, service, before, after)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $5::uuid IS NULL THEN NULL ELSE 'aws' END,
		        CASE WHEN $5::uuid IS NULL THEN NULL ELSE 'rds' END, $6, $7)`,
		id, eventType, parent, actor, account, before, after)
	return err
}

func m147TableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('config_audit_events') IS NOT NULL`).Scan(&exists))
	return exists
}

// TestMigration_ConfigAuditEvents covers 000147 (issue #1493): the table
// accepts the three override event shapes, refuses malformed ones, and the
// down migration drops it.
func TestMigration_ConfigAuditEvents(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	const version = 147
	prev := previousMigrationVersion(t, version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.False(t, m147TableExists(ctx, t, pool))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))

	const row = `{"term": 1}`
	require.NoError(t, m147Insert(ctx, pool, m147Import, "account_service_override.import", nil, nil, nil, nil, "admin@example.com"))
	require.NoError(t, m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000b1", "account_service_override.upsert", m147Import, m147Account, nil, row, "admin@example.com"))
	require.NoError(t, m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000b2", "account_service_override.delete", nil, m147Account, row, nil, "admin@example.com"))

	rejected := map[string]func() error{
		"unknown event type": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c1", "global_config.update", nil, m147Account, nil, row, "admin@example.com")
		},
		"empty actor": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c2", "account_service_override.upsert", nil, m147Account, nil, row, "")
		},
		"override event without its override": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c3", "account_service_override.upsert", nil, nil, nil, row, "admin@example.com")
		},
		"upsert without after": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c4", "account_service_override.upsert", nil, m147Account, row, nil, "admin@example.com")
		},
		"delete with an after": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c5", "account_service_override.delete", nil, m147Account, row, row, "admin@example.com")
		},
		"import under a parent": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c6", "account_service_override.import", m147Import, nil, nil, nil, "admin@example.com")
		},
		"parent that does not exist": func() error {
			return m147Insert(ctx, pool, "14700000-0000-4000-8000-0000000000c7", "account_service_override.upsert", "14700000-0000-4000-8000-0000000000ff", m147Account, nil, row, "admin@example.com")
		},
	}
	for name, insert := range rejected {
		assert.Error(t, insert(), name)
	}

	var seqs []int64
	rows, err := pool.Query(ctx, `SELECT seq FROM config_audit_events ORDER BY seq`)
	require.NoError(t, err)
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	require.NoError(t, rows.Err())
	require.Len(t, seqs, 3)
	assert.Less(t, seqs[0], seqs[1])
	assert.Less(t, seqs[1], seqs[2])

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.False(t, m147TableExists(ctx, t, pool), "down drops the table")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	assert.True(t, m147TableExists(ctx, t, pool), "re-up recreates it")
}
