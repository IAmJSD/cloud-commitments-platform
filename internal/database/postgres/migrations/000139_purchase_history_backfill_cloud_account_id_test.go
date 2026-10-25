//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migration139Version = 139

// migration139LockWaitLike matches the migration in pg_stat_activity, which
// truncates query text at 1024 bytes, so it matches the file's first line.
const migration139LockWaitLike = "%Backfill purchase_history.cloud_account_id%"

// Synthetic external ids. The shared number is registered under both aws and
// azure so the tests can prove a row only ever joins its own provider.
const (
	backfillSharedExt  = "111122223333"
	backfillOtherExt   = "444455556666"
	backfillDeletedExt = "777788889999"
	backfillUnknownExt = "555566667777"
	backfillReRegExt   = "666677778888"
	backfillAzureSub   = "8f0e2b6a-3c1d-4e5f-9a7b-1c2d3e4f5a6b"
	backfillGCPProject = "cudly-backfill-project"
)

// insertBackfillAccount registers a cloud account and returns its id.
func insertBackfillAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provider, externalID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO cloud_accounts (name, provider, external_id)
		VALUES ($1, $2, $3) RETURNING id::text`,
		"backfill "+provider+"/"+externalID, provider, externalID).Scan(&id))
	return id
}

// insertBackfillHistory inserts one purchase_history row the way
// SavePurchaseHistory does; cloudAccountID nil stores NULL.
func insertBackfillHistory(ctx context.Context, pool *pgxpool.Pool, purchaseID, provider, externalID string, cloudAccountID *string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, term, payment, cloud_account_id
		) VALUES ($1, $2, now(), $3, 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', $4)`,
		externalID, purchaseID, provider, cloudAccountID)
	return err
}

// historyCloudAccount returns the row's cloud_account_id, "" when NULL.
func historyCloudAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(cloud_account_id::text, '') FROM purchase_history WHERE purchase_id = $1`, purchaseID).Scan(&id))
	return id
}

// runMigration139UpSQL executes the up file directly, outside golang-migrate,
// to prove a second run is safe, and returns the rows it changed.
func runMigration139UpSQL(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	upSQL, err := os.ReadFile(filepath.Join(getMigrationsPath(), "000139_purchase_history_backfill_cloud_account_id.up.sql"))
	require.NoError(t, err)
	tag, err := pool.Exec(ctx, string(upSQL))
	require.NoError(t, err)
	return tag.RowsAffected()
}

// requireMigratedTo139 fails unless the schema is at 000139 and not dirty.
func requireMigratedTo139(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	version, dirty, err := migrations.GetMigrationVersion(ctx, pool, getMigrationsPath())
	require.NoError(t, err)
	assert.Equal(t, uint(migration139Version), version)
	assert.False(t, dirty, "000139 must not leave the schema dirty")
}

// backfillCase is one seeded row: before is the stored cloud_account_id
// (nil stores NULL) and want the expected value afterwards ("" for NULL).
type backfillCase struct {
	purchaseID, provider, externalID string
	before                           *string
	want                             string
}

// TestMigration139_BackfillsCloudAccountID locks down migration 000139
// (issue #22): every purchase_history row with a NULL cloud_account_id whose
// (provider, account_id) names exactly one registered account points at that
// account afterwards, and every other row keeps what it had.
//
// Fail-before: with the UPDATE removed from the up file every "want account"
// row stays NULL. Matching on external_id alone sends azure-same-ext and
// gcp-same-ext to the wrong account (or makes the join ambiguous), and
// dropping the IS NULL guard rewrites aws-already-other.
func TestMigration139_BackfillsCloudAccountID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration139Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	awsAcct := insertBackfillAccount(ctx, t, pool, "aws", backfillSharedExt)
	azureSameExt := insertBackfillAccount(ctx, t, pool, "azure", backfillSharedExt)
	awsOther := insertBackfillAccount(ctx, t, pool, "aws", backfillOtherExt)
	azureSub := insertBackfillAccount(ctx, t, pool, "azure", backfillAzureSub)
	gcpProject := insertBackfillAccount(ctx, t, pool, "gcp", backfillGCPProject)
	deleted := insertBackfillAccount(ctx, t, pool, "aws", backfillDeletedExt)
	reRegOld := insertBackfillAccount(ctx, t, pool, "aws", backfillReRegExt)
	// A degenerate registration with an empty external id: a row whose
	// account_id is empty (ambient STS lookup failed) must not join it.
	_ = insertBackfillAccount(ctx, t, pool, "aws", "")

	cases := []backfillCase{
		{"aws-null", "aws", backfillSharedExt, nil, awsAcct},
		// Legacy provider casing: cloud_accounts.provider is always lowercase.
		{"aws-upper-null", "AWS", backfillSharedExt, nil, awsAcct},
		// Same external number under another provider joins that provider only.
		{"azure-same-ext", "azure", backfillSharedExt, nil, azureSameExt},
		// No gcp account has this number: it must not borrow the aws or azure one.
		{"gcp-same-ext", "gcp", backfillSharedExt, nil, ""},
		// Unknown provider: ambiguous between aws and azure, left NULL.
		{"empty-provider", "", backfillSharedExt, nil, ""},
		{"azure-subscription", "azure", backfillAzureSub, nil, azureSub},
		// External ids are compared exactly, as the read predicate does.
		{"azure-subscription-upper", "azure", strings.ToUpper(backfillAzureSub), nil, ""},
		{"gcp-project", "gcp", backfillGCPProject, nil, gcpProject},
		{"aws-unregistered", "aws", backfillUnknownExt, nil, ""},
		{"aws-empty-account-id", "aws", "", nil, ""},
		// The account was deleted: no cloud_accounts row matches any more.
		{"aws-deleted-null", "aws", backfillDeletedExt, nil, ""},
		{"aws-deleted-stamped", "aws", backfillDeletedExt, &deleted, ""},
		// Already set: never touched, even when it disagrees with account_id.
		{"aws-already-same", "aws", backfillSharedExt, &awsAcct, awsAcct},
		{"aws-already-other", "aws", backfillSharedExt, &awsOther, awsOther},
	}
	for _, c := range cases {
		require.NoError(t, insertBackfillHistory(ctx, pool, c.purchaseID, c.provider, c.externalID, c.before), c.purchaseID)
	}
	require.NoError(t, insertBackfillHistory(ctx, pool, "aws-reregistered", "aws", backfillReRegExt, &reRegOld))
	// Deleting an account clears its stamped rows (ON DELETE SET NULL).
	_, err := pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = ANY($1::uuid[])`, []string{deleted, reRegOld})
	require.NoError(t, err)
	// Re-registering the same (provider, external id) mints a new id. The row
	// joins it: a filter on the new account already returns the row through
	// its account_id, so the backfill only makes that explicit.
	reRegNew := insertBackfillAccount(ctx, t, pool, "aws", backfillReRegExt)

	assertBackfill := func(t *testing.T, stage string) {
		t.Helper()
		for _, c := range cases {
			assert.Equal(t, c.want, historyCloudAccount(ctx, t, pool, c.purchaseID), "%s: %s", stage, c.purchaseID)
		}
		assert.Equal(t, reRegNew, historyCloudAccount(ctx, t, pool, "aws-reregistered"), "%s: aws-reregistered", stage)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration139Version))
	requireMigratedTo139(ctx, t, pool)
	assertBackfill(t, "after 000139")

	// Idempotent: a second run of the same SQL changes nothing.
	assert.Zero(t, runMigration139UpSQL(ctx, t, pool), "a rerun must not rewrite any row")
	assertBackfill(t, "after a second run")

	// The down migration is a no-op: it applies cleanly and keeps the values,
	// and applying the up again (the real "run up twice" path) is clean too.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	assertBackfill(t, "after rollback")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration139Version))
	requireMigratedTo139(ctx, t, pool)
	assertBackfill(t, "after re-applying 000139")
}

// TestMigration139_ConcurrentAccountDelete covers an account being deleted
// while the backfill runs. DeleteCloudAccount locks the account FOR UPDATE and
// deletes it in one transaction. The migration waits for that transaction,
// then skips the deleted account and backfills the rest. Without the FOR KEY
// SHARE lock the UPDATE would join the account from its snapshot and then
// fail the foreign-key check once the delete commits, aborting the migration.
func TestMigration139_ConcurrentAccountDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration139Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	kept := insertBackfillAccount(ctx, t, pool, "aws", backfillSharedExt)
	doomed := insertBackfillAccount(ctx, t, pool, "aws", backfillDeletedExt)
	require.NoError(t, insertBackfillHistory(ctx, pool, "kept-null", "aws", backfillSharedExt, nil))
	require.NoError(t, insertBackfillHistory(ctx, pool, "doomed-null", "aws", backfillDeletedExt, nil))

	del, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = del.Rollback(ctx) }()
	_, err = del.Exec(ctx, `SELECT 1 FROM cloud_accounts WHERE id = $1 FOR UPDATE`, doomed)
	require.NoError(t, err)
	_, err = del.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, doomed)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, migrationsPath, migration139Version) }()

	testhelpers.WaitForLockWait(ctx, t, pool, migration139LockWaitLike)
	require.NoError(t, del.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done), "a concurrent account delete must not abort 000139")
	requireMigratedTo139(ctx, t, pool)

	assert.Equal(t, kept, historyCloudAccount(ctx, t, pool, "kept-null"))
	assert.Empty(t, historyCloudAccount(ctx, t, pool, "doomed-null"),
		"a row whose account was deleted mid-migration stays NULL")
}

// TestMigration139_ConcurrentHistoryWriters runs purchase_history writers
// during the backfill:
//   - a listing stamp (UpdatePurchaseHistoryListing) holding a matched row's
//     lock: the backfill waits for it, then fills the row without losing the
//     stamp;
//   - a writer that sets cloud_account_id first: its value wins;
//   - inserts (SavePurchaseHistory), one made while the backfill is blocked
//     and a stream running the whole time: none is blocked or fails. Rows
//     committed after the UPDATE started are not visible to it and keep the
//     NULL their writer stored; a rerun picks them up.
func TestMigration139_ConcurrentHistoryWriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration139Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	acct := insertBackfillAccount(ctx, t, pool, "aws", backfillSharedExt)
	other := insertBackfillAccount(ctx, t, pool, "aws", backfillOtherExt)
	require.NoError(t, insertBackfillHistory(ctx, pool, "listed", "aws", backfillSharedExt, nil))
	require.NoError(t, insertBackfillHistory(ctx, pool, "stamped-first", "aws", backfillSharedExt, nil))

	listing, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = listing.Rollback(ctx) }()
	_, err = listing.Exec(ctx, `UPDATE purchase_history SET listing_id = 'listing-1', listing_state = 'active'
		WHERE purchase_id = 'listed'`)
	require.NoError(t, err)

	stamp, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = stamp.Rollback(ctx) }()
	_, err = stamp.Exec(ctx, `UPDATE purchase_history SET cloud_account_id = $1 WHERE purchase_id = 'stamped-first'`, other)
	require.NoError(t, err)

	stream := startHistoryInsertStream(ctx, pool, acct)

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, migrationsPath, migration139Version) }()
	testhelpers.WaitForLockWait(ctx, t, pool, migration139LockWaitLike)

	// While the backfill holds its locks, an insert still goes straight through.
	insertCtx, insertCancel := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, insertBackfillHistory(insertCtx, pool, "inserted-during", "aws", backfillSharedExt, nil),
		"000139 must not block purchase_history inserts")
	insertCancel()

	require.NoError(t, listing.Commit(ctx))
	require.NoError(t, stamp.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))
	requireMigratedTo139(ctx, t, pool)
	inserted, streamErr := stream.stop()
	require.NoError(t, streamErr, "concurrent inserts must all succeed")
	require.Positive(t, inserted, "the insert stream must have run during the migration")

	assert.Equal(t, acct, historyCloudAccount(ctx, t, pool, "listed"))
	var listingID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT listing_id FROM purchase_history WHERE purchase_id = 'listed'`).Scan(&listingID))
	assert.Equal(t, "listing-1", listingID, "the backfill must not lose a concurrent listing stamp")
	assert.Equal(t, other, historyCloudAccount(ctx, t, pool, "stamped-first"),
		"a cloud_account_id committed while 000139 waited must not be overwritten")
	assert.Empty(t, historyCloudAccount(ctx, t, pool, "inserted-during"),
		"a row committed after the UPDATE started is outside its snapshot")

	// Every streamed row is NULL (committed too late) or the stamp its writer
	// gave it, never some other account.
	var wrong int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history
		WHERE purchase_id LIKE 'stream-%' AND cloud_account_id IS NOT NULL AND cloud_account_id <> $1`, acct).Scan(&wrong))
	assert.Zero(t, wrong)

	// A rerun converges the late NULL rows; the writer's stamp still stands.
	runMigration139UpSQL(ctx, t, pool)
	assert.Equal(t, acct, historyCloudAccount(ctx, t, pool, "inserted-during"))
	assert.Equal(t, other, historyCloudAccount(ctx, t, pool, "stamped-first"))
	var stillNull int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history
		WHERE purchase_id LIKE 'stream-%' AND cloud_account_id IS NULL`).Scan(&stillNull))
	assert.Zero(t, stillNull)
}

// historyInsertStream inserts purchase_history rows in a loop until stopped,
// alternating NULL and stamped cloud_account_id like the real write paths.
type historyInsertStream struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	count  int
	err    error
}

func startHistoryInsertStream(ctx context.Context, pool *pgxpool.Pool, acct string) *historyInsertStream {
	streamCtx, cancel := context.WithCancel(ctx)
	s := &historyInsertStream{cancel: cancel}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for i := 0; streamCtx.Err() == nil; i++ {
			var cloudAccountID *string
			if i%2 == 1 {
				cloudAccountID = &acct
			}
			err := insertBackfillHistory(streamCtx, pool, fmt.Sprintf("stream-%d", i), "aws", backfillSharedExt, cloudAccountID)
			if err != nil {
				if streamCtx.Err() == nil {
					s.err = err
				}
				return
			}
			s.count++
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return s
}

func (s *historyInsertStream) stop() (int, error) {
	s.cancel()
	s.wg.Wait()
	return s.count, s.err
}
