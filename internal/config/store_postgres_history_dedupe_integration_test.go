//go:build integration

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func historyFixture(accountID, purchaseID string) *PurchaseHistoryRecord {
	cost := 100.0
	return &PurchaseHistoryRecord{
		AccountID: accountID, PurchaseID: purchaseID, Timestamp: time.Now().Truncate(time.Microsecond),
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: &cost,
	}
}

// TestSavePurchaseHistory_RedriveDoesNotDuplicate is MON-06 (#704): a re-drive
// or root retry that adopts an existing commitment saves the same
// (provider, account_id, purchase_id) again. Two rows would double-count spend
// and savings everywhere purchase_history is summed.
func TestSavePurchaseHistory_RedriveDoesNotDuplicate(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	count := func(purchaseID string) int {
		var n int
		require.NoError(t, conn.Pool().QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE purchase_id = $1`, purchaseID).Scan(&n))
		return n
	}

	require.NoError(t, store.SavePurchaseHistory(ctx, historyFixture("123456789012", "ri-0001")))
	require.NoError(t, store.SavePurchaseHistory(ctx, historyFixture("123456789012", "ri-0001")), "a repeated save is a no-op, not an error")
	assert.Equal(t, 1, count("ri-0001"), "re-drive must not add a second history row")

	t.Run("same purchase id in another account is a different purchase", func(t *testing.T) {
		require.NoError(t, store.SavePurchaseHistory(ctx, historyFixture("999999999999", "ri-0001")))
		assert.Equal(t, 2, count("ri-0001"))
	})

	t.Run("same purchase id under another provider is a different purchase", func(t *testing.T) {
		other := historyFixture("123456789012", "ri-0001")
		other.Provider = "azure"
		require.NoError(t, store.SavePurchaseHistory(ctx, other))
		assert.Equal(t, 3, count("ri-0001"))
	})

	t.Run("a row that predates the dedupe index still blocks a repeat", func(t *testing.T) {
		_, err := conn.Pool().Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, created_at)
			VALUES ('123456789012', 'ri-old', now(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', now() - interval '30 days')`)
		require.NoError(t, err)
		require.NoError(t, store.SavePurchaseHistory(ctx, historyFixture("123456789012", "ri-old")))
		assert.Equal(t, 1, count("ri-old"))
	})
}

// TestSavePurchaseHistory_CreatedAtIgnoresPurchaseTimestamp: the unique index
// only covers rows whose created_at is after the migration. created_at must
// therefore come from the column default (now()) and never from the record's
// Timestamp, which for a re-drive is the original purchase time.
func TestSavePurchaseHistory_CreatedAtIgnoresPurchaseTimestamp(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	rec := historyFixture("123456789012", "ri-backdated")
	rec.Timestamp = time.Now().AddDate(-1, 0, 0)
	require.NoError(t, store.SavePurchaseHistory(ctx, rec))

	var recent bool
	require.NoError(t, conn.Pool().QueryRow(ctx,
		`SELECT created_at > now() - interval '1 minute' FROM purchase_history WHERE purchase_id = 'ri-backdated'`).Scan(&recent))
	assert.True(t, recent, "created_at must be the insert time, not the purchase timestamp")
}

// TestSavePurchaseHistory_ConcurrentSaveWaitsForUncommittedTwin forces the
// interleaving the NOT EXISTS check cannot see: another transaction has
// inserted the same key but not committed. The save must wait on the unique
// index and, once the twin commits, do nothing.
func TestSavePurchaseHistory_ConcurrentSaveWaitsForUncommittedTwin(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	tx, err := conn.Pool().Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment)
		VALUES ('123456789012', 'ri-race', now(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront')`)
	require.NoError(t, err)

	holderPID := testhelpers.BackendPID(ctx, t, tx)

	done := make(chan error, 1)
	go func() { done <- store.SavePurchaseHistory(ctx, historyFixture("123456789012", "ri-race")) }()

	if err, answered := testhelpers.AwaitBlockedBy(ctx, t, conn.Pool(), holderPID, done); answered {
		_ = tx.Rollback(ctx)
		t.Fatalf("save returned (%v) while a twin was uncommitted; it must wait on the unique index", err)
	}
	require.NoError(t, tx.Commit(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("save did not finish after the twin committed")
	}

	var n int
	require.NoError(t, conn.Pool().QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE purchase_id = 'ri-race'`).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestMigration108_AppliesOverExistingDuplicates: duplicates that predate the
// migration must not make it fail (no rows are deleted), and the index it
// creates must still reject a duplicate among new rows.
func TestMigration108_AppliesOverExistingDuplicates(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	ctx := context.Background()
	pool := conn.Pool()

	_, err := pool.Exec(ctx, `DROP INDEX uq_purchase_history_purchase`)
	require.NoError(t, err)
	for range 2 {
		_, err = pool.Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, created_at)
			VALUES ('123456789012', 'ri-dup', now(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', now() - interval '1 day')`)
		require.NoError(t, err)
	}
	up, err := os.ReadFile(filepath.Join(getTestMigrationsPath(), "000108_purchase_history_unique_purchase.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "pre-existing duplicates must not block the migration")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE purchase_id = 'ri-dup'`).Scan(&n))
	assert.Equal(t, 2, n, "the migration deletes nothing")

	insertNew := `INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment)
		VALUES ('123456789012', 'ri-new', now(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront')`
	_, err = pool.Exec(ctx, insertNew)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertNew)
	require.Error(t, err, "the index rejects a duplicate among rows created after the migration")
}
