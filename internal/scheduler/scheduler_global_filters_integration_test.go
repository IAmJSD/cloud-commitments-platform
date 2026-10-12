//go:build integration

package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalFiltersPersistedAmbientAndRegistered(t *testing.T) {
	ctx := t.Context()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))
	store := config.NewPostgresStore(pg.DB)
	account := uuid.NewString()
	require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{ID: account, Name: "registered", Provider: "aws", ExternalID: "111111111111", Enabled: true}))
	records := make([]config.RecommendationRecord, 0, 10)
	for i, id := range []*string{nil, &account} {
		for _, count := range []int{1, 10, 11} {
			records = append(records, config.RecommendationRecord{ID: fmt.Sprintf("%d-%d", i, count), Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: fmt.Sprintf("db.m5.%d", count), Engine: "mysql", Count: count, Term: 1, Payment: "all-upfront", CloudAccountID: id})
		}
		records = append(records,
			config.RecommendationRecord{ID: fmt.Sprintf("%d-sibling", i), Provider: "aws", Service: "rds", Region: "us-west-2", ResourceType: "db.r6g.large", Engine: "postgres", Count: 12, Term: 1, Payment: "all-upfront", CloudAccountID: id},
			config.RecommendationRecord{ID: fmt.Sprintf("%d-other", i), Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "all-upfront", CloudAccountID: id})
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now(), records, nil))
	s := &Scheduler{config: store, isLambda: true}
	_, err = store.GetServiceConfig(ctx, "aws", "rds")
	require.ErrorIs(t, err, config.ErrNotFound)
	all, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, all, 10)
	for _, tc := range []struct {
		name   string
		policy config.ServiceConfig
		want   []string
		reason string
	}{
		{"disabled", config.ServiceConfig{}, nil, "enabled=false"},
		{"enabled", config.ServiceConfig{Enabled: true}, []string{"1", "10", "11", "sibling"}, ""},
		{"minimum", config.ServiceConfig{Enabled: true, MinCount: 10}, []string{"10", "11", "sibling"}, ""},
		{"include engine", config.ServiceConfig{Enabled: true, IncludeEngines: []string{"postgres"}}, []string{"sibling"}, "engine"},
		{"exclude engine", config.ServiceConfig{Enabled: true, ExcludeEngines: []string{"mysql"}}, []string{"sibling"}, "engine"},
		{"include region", config.ServiceConfig{Enabled: true, IncludeRegions: []string{"us-west-2"}}, []string{"sibling"}, "region"},
		{"exclude region", config.ServiceConfig{Enabled: true, ExcludeRegions: []string{"us-east-1"}}, []string{"sibling"}, "region"},
		{"include type", config.ServiceConfig{Enabled: true, IncludeTypes: []string{"db.m5.10"}}, []string{"10"}, "resource_type"},
		{"exclude type", config.ServiceConfig{Enabled: true, ExcludeTypes: []string{"db.m5.1"}}, []string{"10", "11", "sibling"}, "resource_type"},
		{"exclude wins", config.ServiceConfig{Enabled: true, IncludeRegions: []string{"us-east-1", "us-west-2"}, ExcludeRegions: []string{"us-east-1"}}, []string{"sibling"}, "region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.policy.Provider, tc.policy.Service, tc.policy.Term, tc.policy.Payment = "aws", "rds", 1, "all-upfront"
			require.NoError(t, store.SaveServiceConfig(ctx, &tc.policy))
			got, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
			require.NoError(t, err)
			ids := make([]string, 0, len(got))
			for _, rec := range got {
				ids = append(ids, rec.ID)
				_, hidden, err := s.GetRecommendationByID(ctx, rec.ID)
				require.NoError(t, err)
				assert.Empty(t, hidden, "visible recommendation %s", rec.ID)
			}
			want := make([]string, 2, 2+2*len(tc.want))
			want[0], want[1] = "0-other", "1-other"
			for _, suffix := range tc.want {
				want = append(want, "0-"+suffix, "1-"+suffix)
			}
			assert.ElementsMatch(t, want, ids)
			for _, id := range []string{"0-1", "1-1"} {
				rec, hidden, err := s.GetRecommendationByID(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, rec)
				if tc.reason != "" {
					assert.Contains(t, hidden, tc.reason)
				} else {
					assert.Empty(t, hidden)
				}
			}
		})
	}
	t.Run("registered override wins", func(t *testing.T) {
		require.NoError(t, store.SaveServiceConfig(ctx, &config.ServiceConfig{Provider: "aws", Service: "rds", Term: 1, Payment: "all-upfront"}))
		require.NoError(t, store.SaveAccountServiceOverride(ctx, &config.AccountServiceOverride{AccountID: account, Provider: "aws", Service: "rds", Enabled: boolPtr(true)}))
		got, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		ids := make([]string, 0, len(got))
		for _, rec := range got {
			ids = append(ids, rec.ID)
		}
		assert.ElementsMatch(t, []string{"0-other", "1-other", "1-1", "1-10", "1-11", "1-sibling"}, ids)
	})
}

// Issue #792, PR2c: persistCollection writes every provider's recommendations
// in one transaction, so two zonal EC2 recs on the same 8-tuple (differing only
// by AZ) used to fail the whole collection write. With the 8-column index gone
// the whole 500-row batch lands, siblings included.
func TestPersistCollectionMultiAZInOneBatch(t *testing.T) {
	ctx := t.Context()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))
	store := config.NewPostgresStore(pg.DB)

	zonal := func(az string) config.RecommendationRecord {
		return config.RecommendationRecord{
			ID: "ec2-" + az, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
			AvailabilityZone: az, Count: 1, Term: 1, Payment: "all-upfront", Savings: 10,
			Details: json.RawMessage(`{"instance_type":"m5.large","scope":"Availability Zone","availability_zone":"` + az + `"}`),
		}
	}
	const total = 500
	recs := []config.RecommendationRecord{zonal("us-east-1a"), zonal("us-east-1b")}
	for i := len(recs); i < total; i++ {
		recs = append(recs, config.RecommendationRecord{
			ID: fmt.Sprintf("filler-%d", i), Provider: "aws", Service: "ec2", Region: "us-west-2",
			ResourceType: fmt.Sprintf("c5.type%d", i), Count: 1, Term: 1, Payment: "all-upfront", Savings: 1,
		})
	}

	s := &Scheduler{config: store}
	s.persistCollection(ctx, recs, []config.SuccessfulCollect{{Provider: "aws"}}, nil)

	rows, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, rows, total, "the whole collection must persist, not fail on the AZ pair")
	azs := map[string]bool{}
	for _, r := range rows {
		if r.ResourceType == "m5.large" {
			azs[r.AvailabilityZone] = true
		}
	}
	assert.Equal(t, map[string]bool{"us-east-1a": true, "us-east-1b": true}, azs)
	state, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, state.LastCollectionError, "persistCollection must not record a write error")
}
