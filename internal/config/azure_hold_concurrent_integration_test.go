//go:build integration
// +build integration

package config_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Two overlapping collections: one commits a pricing hold on X, the other
// (which started earlier and only re-prices Y) commits later. The later commit
// must not drop X's hold. The holder transaction stands in for the first
// collection mid-flight; the second upsert is provably queued behind it before
// the first commits.
func TestUpsertRecommendationsWithPartial_ConcurrentCollectionKeepsHold(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB
	store := config.NewPostgresStore(db)
	acctX, acctY := uuid.NewString(), uuid.NewString()
	seedRecommendationCloudAccount(ctx, t, store, acctX, "azure", "sub-hold-x")
	seedRecommendationCloudAccount(ctx, t, store, acctY, "azure", "sub-hold-y")

	// The hold protects X's stored rows, which must exist for it to carry.
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().Add(-time.Hour),
		[]config.RecommendationRecord{azureAccountRec("row-x", acctX, "D2")}, nil))

	holder, err := db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	_, err = holder.Exec(ctx, `SELECT 1 FROM recommendations_state WHERE id = 1 FOR UPDATE`)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- store.UpsertRecommendationsWithPartial(ctx, time.Now().UTC(), nil,
			[]config.SuccessfulCollect{{Provider: "azure", CloudAccountID: &acctY}}, nil)
	}()
	_, finished := testhelpers.AwaitBlockedBy(ctx, t, db, testhelpers.BackendPID(ctx, t, holder), done)
	require.False(t, finished, "the second collection must queue behind the first")

	hold, err := json.Marshal(&config.RecommendationsCollectionPartial{Providers: []config.PartialProviderSweep{{
		Provider: "azure", IncompleteAccounts: 1, HeldAccountIDs: []string{acctX},
		Reasons: map[string]int{config.PartialReasonPricingUnavailable: 1},
	}}})
	require.NoError(t, err)
	_, err = holder.Exec(ctx, `UPDATE recommendations_state SET last_collection_partial = $1::jsonb WHERE id = 1`, hold)
	require.NoError(t, err)
	require.NoError(t, holder.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	fresh, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.NotNil(t, fresh.LastCollectionPartial, "the later commit dropped the other run's hold")
	require.Equal(t, []string{acctX}, fresh.LastCollectionPartial.Providers[0].HeldAccountIDs)
}

// The stored-rows read must be limited to Azure: an ambient Azure hold ends when
// no Azure ambient rows remain, even if another provider's ambient rows exist.
func TestUpsertRecommendationsWithPartial_AmbientHoldIgnoresOtherProviderRows(t *testing.T) {
	ctx := context.Background()
	store := config.NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB)
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().Add(-time.Hour),
		[]config.RecommendationRecord{ambientRec("aws-ambient", "aws", "m5.large")}, nil))
	ambientHold := &config.RecommendationsCollectionPartial{Providers: []config.PartialProviderSweep{{
		Provider: "azure", IncompleteAccounts: 1, Reasons: map[string]int{config.PartialReasonPricingUnavailable: 1},
	}}}
	require.NoError(t, store.UpsertRecommendationsWithPartial(ctx, time.Now().UTC(), nil, nil, ambientHold))

	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), nil, nil))

	fresh, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.Nil(t, fresh.LastCollectionPartial, "AWS ambient rows must not keep an Azure hold alive")
}
