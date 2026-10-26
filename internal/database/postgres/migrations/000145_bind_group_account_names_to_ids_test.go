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
	m149AcctProd     = "14900000-0000-4000-8000-000000000001"
	m149AcctSharedA  = "14900000-0000-4000-8000-000000000002"
	m149AcctSharedB  = "14900000-0000-4000-8000-000000000003"
	m149AcctReuser   = "14900000-0000-4000-8000-000000000004"
	m149AcctStar     = "14900000-0000-4000-8000-000000000005"
	m149AcctExtTwin  = "14900000-0000-4000-8000-000000000006"
	m149AcctDisabled = "14900000-0000-4000-8000-000000000007"
	m149AcctLater    = "14900000-0000-4000-8000-000000000008"

	// The group was last written at this time; accounts created after it
	// cannot be the account a name entry was granted for.
	m149GroupWritten = "2026-06-01T00:00:00Z"
	m149Before       = "2026-01-01T00:00:00Z"
	m149After        = "2026-09-01T00:00:00Z"
)

func m149InsertAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, name, provider, externalID, createdAt string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO cloud_accounts (id, name, provider, external_id, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::timestamptz)`, id, name, provider, externalID, enabled, createdAt)
	require.NoError(t, err)
}

func m149InsertGroup(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, allowed []string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO groups (name, permissions, allowed_accounts, created_at, updated_at)
		VALUES ($1, '[]', $2, $3::timestamptz, $3::timestamptz)`, name, allowed, m149GroupWritten)
	require.NoError(t, err)
}

func m149Allowed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	var allowed []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT allowed_accounts FROM groups WHERE name = $1`, name).Scan(&allowed))
	return allowed
}

// TestMigration_BindGroupAccountNamesToIDs covers 000145 (issue #1034): name
// entries that identify exactly one account that predates the grant become
// that account's ID; everything else is left untouched.
func TestMigration_BindGroupAccountNamesToIDs(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	const version = 145
	prev := previousMigrationVersion(t, version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))

	m149InsertAccount(ctx, t, pool, m149AcctProd, "prod", "aws", "111111111111", m149Before, true)
	m149InsertAccount(ctx, t, pool, m149AcctSharedA, "shared", "aws", "222222222222", m149Before, true)
	m149InsertAccount(ctx, t, pool, m149AcctSharedB, "shared", "azure", "sub-shared-b", m149Before, false)
	// Registered after the grant under the name of an account that is gone.
	m149InsertAccount(ctx, t, pool, m149AcctReuser, "deleted-acct", "gcp", "proj-reuser", m149After, true)
	// An account whose display name is the wildcard must not narrow "*".
	m149InsertAccount(ctx, t, pool, m149AcctStar, "*", "gcp", "proj-star", m149Before, true)
	// Display name equal to another account's external ID.
	m149InsertAccount(ctx, t, pool, m149AcctExtTwin, "111111111111", "gcp", "proj-twin", m149Before, true)
	m149InsertAccount(ctx, t, pool, m149AcctDisabled, "dormant", "aws", "333333333333", m149Before, false)
	m149InsertAccount(ctx, t, pool, m149AcctLater, "later", "aws", "444444444444", m149After, true)

	m149InsertGroup(ctx, t, pool, "unique-name", []string{"prod"})
	m149InsertGroup(ctx, t, pool, "mixed", []string{m149AcctSharedA, "prod", "dormant", "unknown"})
	m149InsertGroup(ctx, t, pool, "shared-name", []string{"shared"})
	m149InsertGroup(ctx, t, pool, "reused-name", []string{"deleted-acct"})
	m149InsertGroup(ctx, t, pool, "created-later", []string{"later"})
	m149InsertGroup(ctx, t, pool, "wildcard", []string{"*"})
	m149InsertGroup(ctx, t, pool, "external-id", []string{"111111111111"})
	m149InsertGroup(ctx, t, pool, "blank-entry", []string{"", "prod"})
	m149InsertGroup(ctx, t, pool, "empty", []string{})
	_, err := pool.Exec(ctx, `
		INSERT INTO groups (name, permissions, allowed_accounts, updated_at)
		VALUES ('null-list', '[]', NULL, $1::timestamptz)`, m149GroupWritten)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))

	want := map[string][]string{
		"unique-name": {m149AcctProd},
		// Order kept; a disabled account still binds; an unknown name stays.
		"mixed":         {m149AcctSharedA, m149AcctProd, m149AcctDisabled, "unknown"},
		"shared-name":   {"shared"},
		"reused-name":   {"deleted-acct"},
		"created-later": {"later"},
		"wildcard":      {"*"},
		"external-id":   {"111111111111"},
		"blank-entry":   {"", m149AcctProd},
		"empty":         {},
	}
	for group, entries := range want {
		assert.Equal(t, entries, m149Allowed(ctx, t, pool, group), group)
	}
	assert.Nil(t, m149Allowed(ctx, t, pool, "null-list"), "a NULL list stays NULL")

	var updatedAt string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM groups WHERE name = 'unique-name'`).
		Scan(&updatedAt))
	assert.Equal(t, m149GroupWritten, updatedAt,
		"the rewrite must not move updated_at, which the created_at guard reads")

	// Down is a no-op: the IDs stay, and old code matches them by ID.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, []string{m149AcctProd}, m149Allowed(ctx, t, pool, "unique-name"))

	// Re-applying is idempotent.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	for group, entries := range want {
		assert.Equal(t, entries, m149Allowed(ctx, t, pool, group), "after re-up: %s", group)
	}
}
