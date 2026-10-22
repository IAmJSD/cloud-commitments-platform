package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real RDS Details shape as persisted by the scheduler (issue #408).
func rdsDetails(azConfig string) json.RawMessage {
	return json.RawMessage(`{"engine":"mysql","az_config":"` + azConfig + `","instance_class":"db.r5.large"}`)
}

func rdsAZConfigRec(azConfig string, upfront float64) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: "rec-rds-" + azConfig, Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.r5.large",
		Engine: "mysql", Term: 1, Payment: "all-upfront", Count: 1, UpfrontCost: upfront, Savings: 10,
		Details: rdsDetails(azConfig),
	}
}

// Stored Single-AZ and Multi-AZ rows of the same class must both load (no
// "more than one row for identity key" refusal) and the request, which carries
// az_config only in Details, is priced from its own variant's row.
func TestPriceRecommendationsFromStore_PricesFromRequestedAZConfig(t *testing.T) {
	for _, want := range []struct {
		azConfig string
		upfront  float64
	}{
		{"single-az", 100},
		{"multi-az", 300},
	} {
		t.Run(want.azConfig, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			expectStoredRecs(store, rdsAZConfigRec("single-az", 100), rdsAZConfigRec("multi-az", 300))
			h := &Handler{config: store}
			req := []config.RecommendationRecord{{
				Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.r5.large", Engine: "mysql",
				Term: 1, Payment: "all-upfront", Count: 1, Details: rdsDetails(want.azConfig),
			}}
			require.NoError(t, h.priceRecommendationsFromStore(context.Background(), req))
			assert.Equal(t, want.upfront, req[0].UpfrontCost)
			got, err := req[0].DetailsAZConfig()
			require.NoError(t, err)
			assert.Equal(t, want.azConfig, got)
		})
	}
}

// A request that does not name its az_config cannot be priced off an arbitrary
// RDS variant: it is refused with a 409.
func TestPriceRecommendationsFromStore_RequestWithoutAZConfigRefusedForRDS(t *testing.T) {
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	expectStoredRecs(store, rdsAZConfigRec("multi-az", 300))
	h := &Handler{config: store}
	req := []config.RecommendationRecord{{
		Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.r5.large", Engine: "mysql",
		Term: 1, Payment: "all-upfront", Count: 1,
	}}
	err := h.priceRecommendationsFromStore(context.Background(), req)
	require.Error(t, err)
	ce, isClient := IsClientError(err)
	require.True(t, isClient)
	assert.Equal(t, 409, ce.code)
}

func TestPurchaseIdempotencyKey_DiffersByAZConfig(t *testing.T) {
	t.Parallel()
	key := func(azConfig string) string {
		return mustPurchaseIdempotencyKey("u", []config.RecommendationRecord{rdsAZConfigRec(azConfig, 1)}, 100)
	}
	assert.NotEqual(t, key("single-az"), key("multi-az"),
		"a Multi-AZ submit right after a Single-AZ one must not dedupe into it")
	assert.Equal(t, key("multi-az"), key("multi-az"))
}

func TestDescribeRec_IncludesAZConfig(t *testing.T) {
	r := rdsAZConfigRec("multi-az", 1)
	assert.Contains(t, describeRec(&r), `az_config="multi-az"`)
	bare := config.RecommendationRecord{Provider: "aws", Service: "rds"}
	assert.NotContains(t, describeRec(&bare), "az_config=")
}
