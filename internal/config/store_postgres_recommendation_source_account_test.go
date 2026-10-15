//go:build integration

package config_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPostgresRecommendationSourceEncodingRollback(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seed := awsRec("seed", "rds", "region", "seed", 10)
	seed.SourceAccountID = new(" SourceAbC/é\t")
	require.NoError(t, store.UpsertRecommendations(ctx, at, []config.RecommendationRecord{seed}, nil))
	require.NoError(t, store.SetRecommendationsCollectionError(ctx, "preserved"))
	before, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	for _, bad := range []string{"opaque\x00source", string([]byte{'x', 0xff, 'y'})} {
		for _, replace := range []bool{false, true} {
			recs := make([]config.RecommendationRecord, 501)
			for i := range recs {
				recs[i] = awsRec(fmt.Sprint(i), "rds", "region", fmt.Sprint(i), 20)
				recs[i].SourceAccountID = new("valid")
			}
			recs[500].SourceAccountID = &bad
			if replace {
				err = store.ReplaceRecommendations(ctx, at.Add(time.Hour), recs)
			} else {
				err = store.UpsertRecommendations(ctx, at.Add(time.Hour), recs, []config.SuccessfulCollect{{Provider: "aws"}})
			}
			require.EqualError(t, err, "invalid source_account_id encoding")
			rows, listErr := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
			require.NoError(t, listErr)
			require.Len(t, rows, 1)
			require.Equal(t, "seed", rows[0].ID)
			require.Equal(t, seed.SourceAccountID, rows[0].SourceAccountID)
			after, freshnessErr := store.GetRecommendationsFreshness(ctx)
			require.NoError(t, freshnessErr)
			require.Equal(t, before, after)
		}
	}
}
