//go:build integration

package main

// Real PostgreSQL tests for the clear-rate-limit DELETE (issue #357). The
// migrated rate_limits table and the DELETE are real; nothing is deployed and
// no Lambda is invoked.

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// rateLimitRows covers the domain the tests clear, other domains, a literal
// "%" domain, and rows of other endpoints and key kinds. The exampleQtest.com
// and forgotXpassword rows match only while "_" is a wildcard.
var rateLimitRows = []string{
	"EMAIL#alice@example.com#ENDPOINT#forgot_password",
	"EMAIL#bob@example.com#ENDPOINT#forgot_password",
	"EMAIL#dave@other.org#ENDPOINT#forgot_password",
	"EMAIL#erin@%#ENDPOINT#forgot_password",
	"EMAIL#frank@example_test.com#ENDPOINT#forgot_password",
	"EMAIL#grace@exampleQtest.com#ENDPOINT#forgot_password",
	"EMAIL#alice@example.com#ENDPOINT#forgotXpassword",
	"EMAIL#alice@example.com#ENDPOINT#login",
	"IP#192.0.2.1#ENDPOINT#forgot_password_ip",
}

func rateLimitTestDB(ctx context.Context, t *testing.T) *database.Connection {
	t.Helper()
	c := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = c.Cleanup(context.Background()) })
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "internal", "database", "postgres", "migrations")
	require.NoError(t, migrations.RunMigrations(ctx, c.DB.Pool(), path, "", ""))
	return c.DB
}

func seedRateLimits(ctx context.Context, t *testing.T, db *database.Connection) {
	t.Helper()
	_, err := db.Exec(ctx, `DELETE FROM rate_limits`)
	require.NoError(t, err)
	for _, id := range rateLimitRows {
		_, err := db.Exec(ctx, `INSERT INTO rate_limits (id, count, reset_time) VALUES ($1, 1, NOW() + INTERVAL '1 hour')`, id)
		require.NoError(t, err, "insert %s", id)
	}
}

func remainingRateLimits(ctx context.Context, t *testing.T, db *database.Connection) []string {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT id FROM rate_limits`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	sort.Strings(out)
	return out
}

// without returns rateLimitRows minus the given ids, sorted.
func without(deleted ...string) []string {
	gone := map[string]bool{}
	for _, id := range deleted {
		gone[id] = true
	}
	var out []string
	for _, id := range rateLimitRows {
		if !gone[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// TestClearForgotPasswordLimitsDeletesOnlyTheNamedDomain is the issue's
// failure scenario: RATE_LIMIT_DOMAIN=% used to clear the forgot_password rows
// of every domain, and an "_" in the domain matched any one character.
func TestClearForgotPasswordLimitsDeletesOnlyTheNamedDomain(t *testing.T) {
	ctx := context.Background()
	db := rateLimitTestDB(ctx, t)

	cases := []struct {
		domain  string
		deleted []string
	}{
		{"example.com", []string{
			"EMAIL#alice@example.com#ENDPOINT#forgot_password",
			"EMAIL#bob@example.com#ENDPOINT#forgot_password",
		}},
		{"%", []string{"EMAIL#erin@%#ENDPOINT#forgot_password"}},
		{"example_test.com", []string{"EMAIL#frank@example_test.com#ENDPOINT#forgot_password"}},
		{"nobody.invalid", nil},
	}
	for _, tc := range cases {
		t.Run(tc.domain, func(t *testing.T) {
			seedRateLimits(ctx, t, db)
			resp, err := clearForgotPasswordLimits(ctx, db, tc.domain)
			require.NoError(t, err)
			want := without(tc.deleted...)
			require.Equal(t, want, remainingRateLimits(ctx, t, db))
			require.Equal(t, len(tc.deleted), resp.DeletedCount)
			require.Equal(t, len(want), resp.RemainingCount)
		})
	}
}
