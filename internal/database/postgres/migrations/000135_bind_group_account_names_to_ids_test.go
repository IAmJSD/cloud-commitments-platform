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
	m135AcctProd     = "13500000-0000-4000-8000-000000000001"
	m135AcctSharedA  = "13500000-0000-4000-8000-000000000002"
	m135AcctSharedB  = "13500000-0000-4000-8000-000000000003"
	m135AcctReuser   = "13500000-0000-4000-8000-000000000004"
	m135AcctStar     = "13500000-0000-4000-8000-000000000005"
	m135AcctExtTwin  = "13500000-0000-4000-8000-000000000006"
	m135AcctDisabled = "13500000-0000-4000-8000-000000000007"
	m135AcctLater    = "13500000-0000-4000-8000-000000000008"

	// The group was last written at this time; accounts created after it
	// cannot be the account a name entry was granted for.
	m135GroupWritten = "2026-06-01T00:00:00Z"
	m135Before       = "2026-01-01T00:00:00Z"
	m135After        = "2026-09-01T00:00:00Z"
)

func m135InsertAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, name, provider, externalID, createdAt string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO cloud_accounts (id, name, provider, external_id, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::timestamptz)`, id, name, provider, externalID, enabled, createdAt)
	require.NoError(t, err)
}

func m135InsertGroup(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, allowed []string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO groups (name, permissions, allowed_accounts, created_at, updated_at)
		VALUES ($1, '[]', $2, $3::timestamptz, $3::timestamptz)`, name, allowed, m135GroupWritten)
	require.NoError(t, err)
}

func m135Allowed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	var allowed []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT allowed_accounts FROM groups WHERE name = $1`, name).Scan(&allowed))
	return allowed
}

// TestMigration_BindGroupAccountNamesToIDs covers 000135 (issue #1034): name
// entries that identify exactly one account that predates the grant become
// that account's ID; everything else is left untouched.
func TestMigration_BindGroupAccountNamesToIDs(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	const version = 135
	prev := previousMigrationVersion(t, version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))

	m135InsertAccount(ctx, t, pool, m135AcctProd, "prod", "aws", "111111111111", m135Before, true)
	m135InsertAccount(ctx, t, pool, m135AcctSharedA, "shared", "aws", "222222222222", m135Before, true)
	m135InsertAccount(ctx, t, pool, m135AcctSharedB, "shared", "azure", "sub-shared-b", m135Before, false)
	// Registered after the grant under the name of an account that is gone.
	m135InsertAccount(ctx, t, pool, m135AcctReuser, "deleted-acct", "gcp", "proj-reuser", m135After, true)
	// An account whose display name is the wildcard must not narrow "*".
	m135InsertAccount(ctx, t, pool, m135AcctStar, "*", "gcp", "proj-star", m135Before, true)
	// Display name equal to another account's external ID.
	m135InsertAccount(ctx, t, pool, m135AcctExtTwin, "111111111111", "gcp", "proj-twin", m135Before, true)
	m135InsertAccount(ctx, t, pool, m135AcctDisabled, "dormant", "aws", "333333333333", m135Before, false)
	m135InsertAccount(ctx, t, pool, m135AcctLater, "later", "aws", "444444444444", m135After, true)

	m135InsertGroup(ctx, t, pool, "unique-name", []string{"prod"})
	m135InsertGroup(ctx, t, pool, "mixed", []string{m135AcctSharedA, "prod", "dormant", "unknown"})
	m135InsertGroup(ctx, t, pool, "shared-name", []string{"shared"})
	m135InsertGroup(ctx, t, pool, "reused-name", []string{"deleted-acct"})
	m135InsertGroup(ctx, t, pool, "created-later", []string{"later"})
	m135InsertGroup(ctx, t, pool, "wildcard", []string{"*"})
	m135InsertGroup(ctx, t, pool, "external-id", []string{"111111111111"})
	m135InsertGroup(ctx, t, pool, "blank-entry", []string{"", "prod"})
	m135InsertGroup(ctx, t, pool, "empty", []string{})
	_, err := pool.Exec(ctx, `
		INSERT INTO groups (name, permissions, allowed_accounts, updated_at)
		VALUES ('null-list', '[]', NULL, $1::timestamptz)`, m135GroupWritten)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))

	want := map[string][]string{
		"unique-name": {m135AcctProd},
		// Order kept; a disabled account still binds; an unknown name stays.
		"mixed":         {m135AcctSharedA, m135AcctProd, m135AcctDisabled, "unknown"},
		"shared-name":   {"shared"},
		"reused-name":   {"deleted-acct"},
		"created-later": {"later"},
		"wildcard":      {"*"},
		"external-id":   {"111111111111"},
		"blank-entry":   {"", m135AcctProd},
		"empty":         {},
	}
	for group, entries := range want {
		assert.Equal(t, entries, m135Allowed(ctx, t, pool, group), group)
	}
	assert.Nil(t, m135Allowed(ctx, t, pool, "null-list"), "a NULL list stays NULL")

	var updatedAt string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM groups WHERE name = 'unique-name'`).
		Scan(&updatedAt))
	assert.Equal(t, m135GroupWritten, updatedAt,
		"the rewrite must not move updated_at, which the created_at guard reads")

	// Down is a no-op: the IDs stay, and old code matches them by ID.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, []string{m135AcctProd}, m135Allowed(ctx, t, pool, "unique-name"))

	// Re-applying is idempotent.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	for group, entries := range want {
		assert.Equal(t, entries, m135Allowed(ctx, t, pool, group), "after re-up: %s", group)
	}
}
