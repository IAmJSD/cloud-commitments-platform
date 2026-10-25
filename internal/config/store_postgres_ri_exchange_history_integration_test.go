//go:build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGetRIExchangeHistoryForAccounts_Integration pins the store contract the
// scoped history endpoint relies on (issue #308): the account predicate runs
// before the LIMIT, the since bound still applies, and an empty or nil id
// list matches nothing rather than everything.
func TestGetRIExchangeHistoryForAccounts_Integration(t *testing.T) {
	conn := setupTestContainerDB(t)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	seed := `
		INSERT INTO ri_exchange_history (account_id, region, source_ri_ids, source_instance_type,
			source_count, target_offering_id, target_instance_type, target_count, status, created_at)
		SELECT $1, 'us-east-1', ARRAY['ri-' || g], 'm5.large', 1, 'offering', 'm6i.large', 1, 'completed',
			now() - make_interval(days => $3::int, mins => g)
		FROM generate_series(1, $2::int) AS g`
	_, err := conn.Pool().Exec(ctx, seed, "111122223333", 5, 0)
	require.NoError(t, err)
	_, err = conn.Pool().Exec(ctx, seed, "222233334444", 2, 1)
	require.NoError(t, err)
	_, err = conn.Pool().Exec(ctx, seed, "222233334444", 1, 30)
	require.NoError(t, err)

	since := time.Now().AddDate(0, 0, -7)

	recs, err := store.GetRIExchangeHistoryForAccounts(ctx, since, 3, []string{"222233334444"})
	require.NoError(t, err)
	require.Len(t, recs, 2, "limit counts only matching rows, and the since bound still applies")
	for i := range recs {
		require.Equal(t, "222233334444", recs[i].AccountID)
	}
	require.True(t, recs[0].CreatedAt.After(recs[1].CreatedAt), "newest first")

	recs, err = store.GetRIExchangeHistoryForAccounts(ctx, since, 3, []string{"111122223333", "222233334444"})
	require.NoError(t, err)
	require.Len(t, recs, 3)
	for i := range recs {
		require.Equal(t, "111122223333", recs[i].AccountID, "the newest rows win across the requested accounts")
	}

	for name, ids := range map[string][]string{"empty": {}, "nil": nil} {
		recs, err = store.GetRIExchangeHistoryForAccounts(ctx, since, 3, ids)
		require.NoError(t, err, name)
		require.Empty(t, recs, "%s id list must match no rows", name)
		require.NotNil(t, recs, "%s id list must return an empty slice, not nil", name)
	}
}
