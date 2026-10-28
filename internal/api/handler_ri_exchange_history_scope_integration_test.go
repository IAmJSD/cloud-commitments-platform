//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRIExchangeHistory_ScopedUserSeesOlderOwnRows_Integration is the
// regression test for issue #308, on real Postgres through the router. Account
// A owns the 500 newest exchanges of the past year; account B's three are
// older. The history endpoint used to fetch the newest 500 rows of every
// account and filter by scope afterwards, so a user scoped to B got an empty
// list. The account predicate now runs in SQL before the LIMIT.
func TestRIExchangeHistory_ScopedUserSeesOlderOwnRows_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store := config.NewPostgresStore(container.DB)

	acctA := &config.CloudAccount{Name: "exchange-a", Provider: "aws", ExternalID: "111122223333", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acctA))
	acctB := &config.CloudAccount{Name: "exchange-b", Provider: "aws", ExternalID: "222233334444", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acctB))

	// 500 rows for A in the last 500 minutes; 3 rows for B two days back;
	// one more B row outside the one-year window, which must stay hidden.
	seed := `
		INSERT INTO ri_exchange_history (account_id, region, source_ri_ids, source_instance_type,
			source_count, target_offering_id, target_instance_type, target_count, status, created_at)
		SELECT $1, 'us-east-1', ARRAY['ri-' || g], 'm5.large', 1, 'offering', 'm6i.large', 1, 'completed',
			now() - make_interval(days => $3::int, mins => g)
		FROM generate_series(1, $2::int) AS g`
	_, err := container.DB.Pool().Exec(ctx, seed, acctA.ExternalID, 500, 0)
	require.NoError(t, err)
	_, err = container.DB.Pool().Exec(ctx, seed, acctB.ExternalID, 3, 2)
	require.NoError(t, err)
	_, err = container.DB.Pool().Exec(ctx, seed, acctB.ExternalID, 1, 400)
	require.NoError(t, err)

	history := func(t *testing.T, scope ...string) []config.RIExchangeRecord {
		t.Helper()
		mockAuth := new(MockAuthService)
		mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
		mockAuth.grantScoped(scope...)
		router := NewRouter(&Handler{config: store, auth: mockAuth})
		res, err := router.Route(ctx, "GET", "/api/ri-exchange/history", ladderScopedReq(""))
		require.NoError(t, err)
		return res.(*RIExchangeHistoryResponse).Records
	}

	for _, tc := range []struct{ by, scope string }{
		{"uuid", acctB.ID}, {"sts account id", acctB.ExternalID},
	} {
		t.Run("scoped to B by "+tc.by, func(t *testing.T) {
			recs := history(t, tc.scope)
			require.Len(t, recs, 3, "B's in-window exchanges must not be crowded out by A's newer 500")
			for i := range recs {
				require.Equal(t, acctB.ExternalID, recs[i].AccountID)
				if i > 0 {
					require.False(t, recs[i].CreatedAt.After(recs[i-1].CreatedAt), "newest first")
				}
			}
		})
	}

	// A display-name entry is not bound to an account and matches nothing
	// (issue #1034), so it reaches none of B's exchanges.
	t.Run("a name entry for B matches nothing", func(t *testing.T) {
		require.Empty(t, history(t, acctB.Name))
	})

	t.Run("scoped to A and B still caps at 500 newest", func(t *testing.T) {
		recs := history(t, acctA.ID, acctB.ID)
		require.Len(t, recs, 500)
		for i := range recs {
			require.Equal(t, acctA.ExternalID, recs[i].AccountID)
		}
	})

	t.Run("unrestricted keeps the newest 500", func(t *testing.T) {
		recs := history(t)
		require.Len(t, recs, 500)
		for i := range recs {
			require.Equal(t, acctA.ExternalID, recs[i].AccountID)
		}
	})

	t.Run("scoped to an account with no exchanges", func(t *testing.T) {
		require.Empty(t, history(t, "333344445555"))
	})
}
