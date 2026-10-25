//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migration133Version = 133

// insertOfferingClassRow inserts one purchase_history row with the given
// provider, service and offering_class (nil stores NULL).
func insertOfferingClassRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID, provider, service string, offeringClass *string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, term, payment, offering_class
		) VALUES ('test-account', $1, $2, $3, $4, 'us-east-1', 'm5.large', 1, 'all-upfront', $5)`,
		purchaseID, time.Now(), provider, service, offeringClass)
	require.NoError(t, err)
}

// offeringClassOf returns the row's offering_class, nil when NULL.
func offeringClassOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID string) *string {
	t.Helper()
	var class *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT offering_class FROM purchase_history WHERE purchase_id = $1`, purchaseID).Scan(&class))
	return class
}

// TestMigration133_ClearsConvertibleEC2Stamps locks down migration 000133
// (issue #524): before #509 every AWS 'ec2' purchase_history row was stamped
// 'convertible' even when the class setting was 'standard', which blocks
// selling a Standard RI. The migration clears the convertible stamp on AWS EC2
// rows (legacy 'ec2' and canonical 'compute' slugs) so the marketplace handler
// re-fetches the class from AWS, and leaves every other row alone.
//
// Fail-before: with the UPDATE removed from the up file, the cleared rows keep
// 'convertible' and the NULL assertions fail.
func TestMigration133_ClearsConvertibleEC2Stamps(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration133Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	cases := []struct {
		purchaseID, provider, service string
		before, after                 *string
	}{
		// The defect: a stale convertible stamp on an EC2 RI.
		{"ri-ec2-convertible", "aws", "ec2", strPtr("convertible"), nil},
		{"ri-compute-convertible", "aws", "compute", strPtr("convertible"), nil},
		// The sell guard compares case-insensitively, so the reset does too.
		{"ri-ec2-convertible-upper", "aws", "ec2", strPtr("Convertible"), nil},
		// A standard stamp is already sellable and must not be touched.
		{"ri-ec2-standard", "aws", "ec2", strPtr("standard"), strPtr("standard")},
		{"ri-compute-standard", "aws", "compute", strPtr("standard"), strPtr("standard")},
		// Already unknown: stays unknown.
		{"ri-ec2-null", "aws", "ec2", nil, nil},
		// Out of scope: other AWS services and other providers.
		{"ri-rds-convertible", "aws", "rds", strPtr("convertible"), strPtr("convertible")},
		{"azure-compute-convertible", "azure", "compute", strPtr("convertible"), strPtr("convertible")},
	}
	for _, c := range cases {
		insertOfferingClassRow(ctx, t, pool, c.purchaseID, c.provider, c.service, c.before)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration133Version))
	for _, c := range cases {
		assert.Equal(t, c.after, offeringClassOf(ctx, t, pool, c.purchaseID), "after 000133: %s", c.purchaseID)
	}

	// The down migration is a no-op: it must apply cleanly and must not
	// re-stamp cleared rows convertible (that would re-block Standard RIs).
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	for _, c := range cases {
		assert.Equal(t, c.after, offeringClassOf(ctx, t, pool, c.purchaseID), "after rollback: %s", c.purchaseID)
	}
}

// TestMigration133_ConcurrentStandardStampWins covers the marketplace handler
// stamping a row 'standard' from AWS (StampOfferingClass) while the migration
// runs. The migration queues behind the stamp's row lock; once the stamp
// commits, the UPDATE re-checks its WHERE clause on the new row version and
// must leave the authoritative 'standard' in place rather than clearing it.
func TestMigration133_ConcurrentStandardStampWins(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration133Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	const purchaseID = "ri-ec2-race"
	insertOfferingClassRow(ctx, t, pool, purchaseID, "aws", "ec2", strPtr("convertible"))

	stamp, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = stamp.Rollback(ctx) }()
	_, err = stamp.Exec(ctx, `UPDATE purchase_history SET offering_class = $1 WHERE purchase_id = $2`, "standard", purchaseID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, migrationsPath, migration133Version) }()

	// pg_stat_activity truncates query text at 1024 bytes, which cuts the
	// migration file inside its header comment, so match on that comment.
	testhelpers.WaitForLockWait(ctx, t, pool, "%stale EC2 RI offering_class%")
	require.NoError(t, stamp.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	assert.Equal(t, strPtr("standard"), offeringClassOf(ctx, t, pool, purchaseID),
		"a standard stamp committed while 000133 waited must survive the migration")
}
