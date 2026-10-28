//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const commitmentTypeVocabularyVersion = 141

// insertTypedSnapshot inserts one savings_snapshots row and returns the error
// so callers can assert CHECK rejections.
func insertTypedSnapshot(ctx context.Context, pool *pgxpool.Pool, id string, ts time.Time, provider, service, commitmentType string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO savings_snapshots (id, account_id, timestamp, provider, service, region, commitment_type)
		VALUES ($1, 'acct-339', $2, $3, $4, 'us-east-1', $5)`, id, ts, provider, service, commitmentType)
	return err
}

func requireCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a Postgres error, got %v", err)
	require.Equal(t, "23514", pgErr.Code, "want check_violation, got %s: %s", pgErr.Code, pgErr.Message)
	require.Equal(t, "valid_commitment_type", pgErr.ConstraintName)
}

func commitmentTypeOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var v string
	require.NoError(t, pool.QueryRow(ctx, `SELECT commitment_type FROM savings_snapshots WHERE id = $1`, id).Scan(&v))
	return v
}

// TestMigration_SavingsSnapshotsCommitmentTypeVocabulary covers 000141 (#339,
// A06-019): the CHECK admits the pkg/common vocabulary (plus the legacy
// literals), existing rows are relabeled from (provider, service) in every
// partition, and down restores the two-value CHECK.
func TestMigration_SavingsSnapshotsCommitmentTypeVocabulary(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, commitmentTypeVocabularyVersion)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))

	// One month gets its own partition; the other month lands in the default
	// partition, so the relabel and the constraint are checked on both.
	_, err := pool.Exec(ctx, `SELECT create_savings_snapshot_partition('2031-05-15'::date)`)
	require.NoError(t, err)
	inPartition := time.Date(2031, 5, 10, 12, 0, 0, 0, time.UTC)
	inDefault := time.Date(2041, 7, 10, 12, 0, 0, 0, time.UTC)

	// Rows as the pre-fix collector wrote them.
	legacy := []struct {
		id, provider, service, commitmentType string
		ts                                    time.Time
		want                                  string
	}{
		{"00000000-0000-0000-0000-000000003391", "aws", "ec2", "RI", inPartition, "reserved-instance"},
		{"00000000-0000-0000-0000-000000003392", "aws", "savingsplans", "RI", inDefault, "savings-plan"},
		{"00000000-0000-0000-0000-000000003393", "aws", "savings-plans-compute", "RI", inPartition, "savings-plan"},
		{"00000000-0000-0000-0000-000000003394", "aws", "SavingsPlans", "SavingsPlan", inDefault, "savings-plan"},
		{"00000000-0000-0000-0000-000000003395", "gcp", "compute", "RI", inPartition, "committed-use"},
		{"00000000-0000-0000-0000-000000003396", "gcp", "cloudsql", "RI", inDefault, "committed-use"},
		{"00000000-0000-0000-0000-000000003397", "azure", "compute", "RI", inDefault, "reserved-instance"},
	}
	for _, r := range legacy {
		require.NoError(t, insertTypedSnapshot(ctx, pool, r.id, r.ts, r.provider, r.service, r.commitmentType))
	}

	// The bug at the schema level: a GCP CUD cannot be stored as one.
	requireCheckViolation(t, insertTypedSnapshot(ctx, pool,
		"00000000-0000-0000-0000-000000003390", inPartition, "gcp", "compute", "committed-use"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, commitmentTypeVocabularyVersion))

	for _, r := range legacy {
		require.Equal(t, r.want, commitmentTypeOf(ctx, t, pool, r.id), "%s/%s", r.provider, r.service)
	}

	// Every pkg/common value is admitted in both partitions, the legacy
	// literals still are (rolling deploy), and anything else is not.
	n := 0
	for _, ts := range []time.Time{inPartition, inDefault} {
		for _, v := range []string{"reserved-instance", "savings-plan", "committed-use", "reserved-capacity", "RI", "SavingsPlan"} {
			n++
			id := fmt.Sprintf("00000000-0000-0000-0001-%012d", n)
			require.NoError(t, insertTypedSnapshot(ctx, pool, id, ts, "aws", "ec2", v), "%s at %s", v, ts)
		}
		requireCheckViolation(t, insertTypedSnapshot(ctx, pool,
			fmt.Sprintf("00000000-0000-0000-0002-%012d", ts.Year()), ts, "aws", "ec2", "CUD"))
	}

	t.Run("down restores the two-value CHECK and up relabels again", func(t *testing.T) {
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))

		var bad int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM savings_snapshots WHERE commitment_type NOT IN ('RI', 'SavingsPlan')`).Scan(&bad))
		require.Zero(t, bad)
		require.Equal(t, "SavingsPlan", commitmentTypeOf(ctx, t, pool, "00000000-0000-0000-0000-000000003392"))
		require.Equal(t, "RI", commitmentTypeOf(ctx, t, pool, "00000000-0000-0000-0000-000000003395"))
		requireCheckViolation(t, insertTypedSnapshot(ctx, pool,
			"00000000-0000-0000-0003-000000000001", inPartition, "gcp", "compute", "committed-use"))

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, commitmentTypeVocabularyVersion))
		for _, r := range legacy {
			require.Equal(t, r.want, commitmentTypeOf(ctx, t, pool, r.id), "%s/%s", r.provider, r.service)
		}
	})
}
