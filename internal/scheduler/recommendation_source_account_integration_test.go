//go:build integration

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAWSRecommendationSourceAccountPersistence(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(container.DB)
	collector := uuid.NewString()
	require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{
		ID: collector, Name: "collector", Provider: "aws", ExternalID: "collector-account",
	}))
	fixture := &completenessHTTP{resourceType: "db.t3.medium", details: []string{"2", "2", "2", "2", "2"},
		accounts: []any{" SourceAbC/é\t", "sourceabc/e\u0301", nil, ""}}
	client, err := completenessProvider(t, fixture).GetRecommendationsClient(ctx)
	require.NoError(t, err)
	raw, err := client.GetAllRecommendations(ctx)
	require.NoError(t, err)
	require.Len(t, raw, 30)
	s := &Scheduler{}
	records := s.tagAccount(s.convertRecommendations(raw, "aws"), collector)
	require.Len(t, records, 30)
	require.Equal(t, new(" SourceAbC/é\t"), records[0].SourceAccountID)
	require.Equal(t, new("sourceabc/e\u0301"), records[1].SourceAccountID)
	require.Nil(t, records[2].SourceAccountID)
	require.Nil(t, records[3].SourceAccountID)
	require.Nil(t, records[4].SourceAccountID)
	require.Equal(t, &collector, records[0].CloudAccountID)
	at := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.UpsertRecommendations(ctx, at, records[:1], nil))
	rows, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, records[0].SourceAccountID, rows[0].SourceAccountID)
	require.Equal(t, &collector, rows[0].CloudAccountID)
	var key string
	require.NoError(t, container.DB.Pool().QueryRow(ctx, "SELECT source_account_key FROM recommendations").Scan(&key))
	require.Equal(t, *records[0].SourceAccountID, key)
	require.Error(t, store.UpsertRecommendations(ctx, at.Add(time.Hour), records[:2], nil))
	rows, err = store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, records[0].SourceAccountID, rows[0].SourceAccountID)
	for _, req := range fixture.requests {
		if req.Operation == "AWSInsightsIndexService.GetReservationPurchaseRecommendation" {
			require.Equal(t, "LINKED", req.AccountScope)
		}
	}
}
