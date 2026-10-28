//go:build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic external ids (issue #1235). The shared number is registered under
// both aws and azure, so a row must only ever join its own provider's account.
const (
	stampSharedExt  = "111122223333"
	stampAzureSub   = "8f0e2b6a-3c1d-4e5f-9a7b-1c2d3e4f5a6b"
	stampGCPProject = "cudly-stamp-project"
	stampUnknownExt = "555566667777"
)

// registerStampAccount registers a cloud account and returns its id.
func registerStampAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provider, externalID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO cloud_accounts (name, provider, external_id)
		VALUES ($1, $2, $3) RETURNING id::text`,
		"stamp "+provider+"/"+externalID, provider, externalID).Scan(&id))
	return id
}

// storedCloudAccount returns the row's cloud_account_id, "" when NULL.
func storedCloudAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(cloud_account_id::text, '') FROM purchase_history WHERE purchase_id = $1`, purchaseID).Scan(&id))
	return id
}

func stampRecord(provider, accountID, purchaseID string) *PurchaseHistoryRecord {
	rec := historyFixture(accountID, purchaseID)
	rec.Provider = provider
	return rec
}

// TestSavePurchaseHistory_StampsCloudAccountID covers issue #1235: the
// direct-execute, ambient and legacy single-account paths all reach
// SavePurchaseHistory with CloudAccountID nil, and the row used to be stored
// with cloud_account_id NULL. The store now resolves it from
// (provider, account_id) with the same rules as the 000139 backfill.
func TestSavePurchaseHistory_StampsCloudAccountID(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	pool := conn.Pool()
	ctx := context.Background()

	awsAcct := registerStampAccount(ctx, t, pool, "aws", stampSharedExt)
	azureShared := registerStampAccount(ctx, t, pool, "azure", stampSharedExt)
	azureAcct := registerStampAccount(ctx, t, pool, "azure", stampAzureSub)
	gcpAcct := registerStampAccount(ctx, t, pool, "gcp", stampGCPProject)
	// A blank external id must never attract rows with no account id.
	registerStampAccount(ctx, t, pool, "aws", "")

	cases := []struct {
		name string
		rec  *PurchaseHistoryRecord
		want string
	}{
		// Direct execute: resolveSingleAccountProvider stamps the target's
		// ExternalID on AccountID, and the execution has no CloudAccountID.
		{"direct execute azure", stampRecord("azure", stampAzureSub, "ph-direct-azure"), azureAcct},
		{"direct execute gcp", stampRecord("gcp", stampGCPProject, "ph-direct-gcp"), gcpAcct},
		// Ambient: AccountID is the host's STS identity, registered as aws.
		// The same number under azure must not be picked.
		{"ambient aws host account", stampRecord("aws", stampSharedExt, "ph-ambient"), awsAcct},
		{"azure row with the shared number", stampRecord("azure", stampSharedExt, "ph-shared-azure"), azureShared},
		// Legacy rows spell the provider in upper case.
		{"legacy upper-case provider", stampRecord("AWS", stampSharedExt, "ph-legacy"), awsAcct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, store.SavePurchaseHistory(ctx, tc.rec))
			assert.Equal(t, tc.want, storedCloudAccount(ctx, t, pool, tc.rec.PurchaseID))
		})
	}

	t.Run("an explicit cloud account id is kept", func(t *testing.T) {
		rec := stampRecord("aws", stampSharedExt, "ph-explicit")
		rec.CloudAccountID = &gcpAcct
		require.NoError(t, store.SavePurchaseHistory(ctx, rec))
		assert.Equal(t, gcpAcct, storedCloudAccount(ctx, t, pool, rec.PurchaseID))
	})

	// Rows that match no registered account are still stored: dropping a
	// committed purchase's history would be an audit gap (#621). Readers
	// still find them through the external account_id.
	unmatched := []struct {
		name string
		rec  *PurchaseHistoryRecord
	}{
		{"unregistered ambient account", stampRecord("aws", stampUnknownExt, "ph-unregistered")},
		{"empty account id", stampRecord("aws", "", "ph-empty-account")},
		{"empty provider", stampRecord("", stampSharedExt, "ph-empty-provider")},
		{"subscription differing only in case", stampRecord("azure", "8F0E2B6A-3C1D-4E5F-9A7B-1C2D3E4F5A6B", "ph-case")},
	}
	for _, tc := range unmatched {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, store.SavePurchaseHistory(ctx, tc.rec))
			assert.Empty(t, storedCloudAccount(ctx, t, pool, tc.rec.PurchaseID))
		})
	}

	t.Run("a repeated save keeps the existing row", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE purchase_history SET cloud_account_id = NULL WHERE purchase_id = 'ph-ambient'`)
		require.NoError(t, err)
		require.NoError(t, store.SavePurchaseHistory(ctx, stampRecord("aws", stampSharedExt, "ph-ambient")))
		assert.Empty(t, storedCloudAccount(ctx, t, pool, "ph-ambient"), "the dedupe path must not rewrite the existing row")
	})
}

// TestSavePurchaseHistory_ConcurrentAccountDelete: an account deleted while a
// save is resolving it must not abort the save with a foreign-key error (the
// purchase already happened, so a failed save is an audit gap). The lookup
// locks the account FOR KEY SHARE, so the save waits for the delete and then
// stores NULL, the value ON DELETE SET NULL would have left anyway.
func TestSavePurchaseHistory_ConcurrentAccountDelete(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	pool := conn.Pool()
	ctx := context.Background()

	acct := registerStampAccount(ctx, t, pool, "aws", stampSharedExt)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, acct)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- store.SavePurchaseHistory(ctx, stampRecord("aws", stampSharedExt, "ph-race-delete"))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO purchase_history%'`).Scan(&waiting))
		return waiting > 0
	}, 10*time.Second, 20*time.Millisecond, "the save must wait for the in-flight account delete")
	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		require.NoError(t, err, "a concurrent account delete must not fail the history save")
	case <-time.After(10 * time.Second):
		t.Fatal("the save did not finish after the delete committed")
	}
	assert.Empty(t, storedCloudAccount(ctx, t, pool, "ph-race-delete"))
}

// TestSavePurchaseHistory_ConcurrentSavesAndDelete races many saves against an
// account delete. Every save must succeed and store either the account (saved
// first; ON DELETE SET NULL then clears it) or NULL, never fail on the FK.
func TestSavePurchaseHistory_ConcurrentSavesAndDelete(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	pool := conn.Pool()
	ctx := context.Background()

	acct := registerStampAccount(ctx, t, pool, "aws", stampSharedExt)

	const n = 20
	errs := make(chan error, n+1)
	start := make(chan struct{})
	for i := range n {
		go func() {
			<-start
			errs <- store.SavePurchaseHistory(ctx, stampRecord("aws", stampSharedExt, "ph-race-"+string(rune('a'+i))))
		}()
	}
	go func() {
		<-start
		_, err := pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, acct)
		errs <- err
	}()
	close(start)
	for range n + 1 {
		require.NoError(t, <-errs)
	}

	var rows, attached int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), count(cloud_account_id) FROM purchase_history WHERE purchase_id LIKE 'ph-race-%'`).Scan(&rows, &attached))
	assert.Equal(t, n, rows)
	assert.Zero(t, attached, "the deleted account's rows are detached, as ON DELETE SET NULL does")
}
