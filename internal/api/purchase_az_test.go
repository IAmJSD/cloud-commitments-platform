package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real zonal EC2 Details shape as persisted by the scheduler (issue #792).
func zonalEC2Details(az string) json.RawMessage {
	return json.RawMessage(`{"instance_type":"m5.large","platform":"Linux/UNIX","tenancy":"default","scope":"Availability Zone","availability_zone":"` + az + `"}`)
}

func zonalEC2Rec(az string, upfront float64) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: "rec-" + az, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Term: 1, Payment: "all-upfront", Count: 1, UpfrontCost: upfront, Savings: 10,
		AvailabilityZone: az, Details: zonalEC2Details(az),
	}
}

func TestPurchaseIdempotencyKey_DiffersByAvailabilityZone(t *testing.T) {
	t.Parallel()
	a := []config.RecommendationRecord{zonalEC2Rec("us-east-1a", 100)}
	b := []config.RecommendationRecord{zonalEC2Rec("us-east-1b", 100)}
	assert.NotEqual(t, mustPurchaseIdempotencyKey("u", a, 100), mustPurchaseIdempotencyKey("u", b, 100),
		"two submits differing only by AZ must not dedupe into one")
	assert.Equal(t, mustPurchaseIdempotencyKey("u", a, 100), mustPurchaseIdempotencyKey("u", a, 100))
}

func TestPurchaseIdempotencyKey_LegacyRecWithoutTopLevelAZMatchesNewShape(t *testing.T) {
	t.Parallel()
	newShape := zonalEC2Rec("us-east-1a", 100)
	legacy := newShape
	legacy.AvailabilityZone = "" // pending row written before the field existed: AZ only in Details
	assert.Equal(t,
		mustPurchaseIdempotencyKey("u", []config.RecommendationRecord{newShape}, 100),
		mustPurchaseIdempotencyKey("u", []config.RecommendationRecord{legacy}, 100),
		"a double-submit straddling the deploy must still dedupe")
}

func TestPurchaseIdempotencyKey_MalformedComputeDetailsErrorsAndCandidateIsSkipped(t *testing.T) {
	t.Parallel()
	bad := zonalEC2Rec("us-east-1a", 100)
	bad.Details = json.RawMessage(`{"availability_zone":`)
	_, err := purchaseIdempotencyKey("u", []config.RecommendationRecord{bad}, 100)
	require.Error(t, err)

	now := time.Now()
	good := []config.RecommendationRecord{zonalEC2Rec("us-east-1a", 100)}
	key := mustPurchaseIdempotencyKey("u", good, 100)
	creator := "u"
	pending := []config.PurchaseExecution{
		{ExecutionID: "bad", Source: common.PurchaseSourceWeb, ScheduledDate: now, CreatedByUserID: &creator, Recommendations: []config.RecommendationRecord{bad}, CapacityPercent: 100},
		{ExecutionID: "good", Source: common.PurchaseSourceWeb, ScheduledDate: now, CreatedByUserID: &creator, Recommendations: good, CapacityPercent: 100},
	}
	got := matchDuplicateInList(pending, creator, key, now)
	require.NotNil(t, got)
	assert.Equal(t, "good", got.ExecutionID)
}

func TestPurchaseIdempotencyKey_NoDetailsIsEmptyAZ(t *testing.T) {
	t.Parallel()
	r := config.RecommendationRecord{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "all-upfront"}
	_, err := purchaseIdempotencyKey("u", []config.RecommendationRecord{r}, 100)
	require.NoError(t, err)
}

func TestCheckPurchaseDetailIdentity_AvailabilityZoneMismatchRefused(t *testing.T) {
	for name, matchAZ := range map[string]string{"different az": "us-east-1b", "unknown az": ""} {
		t.Run(name, func(t *testing.T) {
			origin := zonalEC2Rec("us-east-1a", 100)
			match := zonalEC2Rec("us-east-1b", 100)
			match.ID = "other"
			if matchAZ == "" {
				match.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"Linux/UNIX","tenancy":"default","scope":"Availability Zone"}`)
			}
			req := config.RecommendationRecord{ID: origin.ID, Service: "ec2", Term: 1, Count: 1}
			err := checkPurchaseDetailIdentity(&origin, &match, &req, 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "availability_zone")
			ce, isClient := IsClientError(err)
			require.True(t, isClient)
			assert.Equal(t, 409, ce.code)
		})
	}
}

func TestCheckPurchaseDetailIdentity_LegacyOriginWithoutAZAllowed(t *testing.T) {
	origin := zonalEC2Rec("us-east-1a", 100)
	origin.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"Linux/UNIX","tenancy":"default","scope":"Availability Zone"}`)
	match := zonalEC2Rec("us-east-1b", 100)
	match.ID = "other"
	req := config.RecommendationRecord{ID: origin.ID, Service: "ec2", Term: 1, Count: 1}
	require.NoError(t, checkPurchaseDetailIdentity(&origin, &match, &req, 0))
}

func TestRecIdentityKey_DiffersByAvailabilityZone(t *testing.T) {
	a, b := zonalEC2Rec("us-east-1a", 1), zonalEC2Rec("us-east-1b", 1)
	assert.NotEqual(t, recIdentityKey(&a), recIdentityKey(&b))
}

// Two zonal rows that differ only by AZ must both load (no "more than one row"
// refusal) and each request must be priced from its own AZ's row.
func TestPriceRecommendationsFromStore_PricesFromRequestedAZ(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	expectStoredRecs(store, zonalEC2Rec("us-east-1a", 100), zonalEC2Rec("us-east-1b", 900))
	h := &Handler{config: store}

	req := []config.RecommendationRecord{{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Term: 1, Payment: "all-upfront", Count: 1, AvailabilityZone: "us-east-1b",
	}}
	require.NoError(t, h.priceRecommendationsFromStore(ctx, req))
	assert.Equal(t, 900.0, req[0].UpfrontCost)
	assert.Equal(t, "us-east-1b", req[0].AvailabilityZone)
}

func TestPriceRecommendationsFromStore_ZonalRequestWithoutAZRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	expectStoredRecs(store, zonalEC2Rec("us-east-1a", 100))
	h := &Handler{config: store}

	req := []config.RecommendationRecord{{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Term: 1, Payment: "all-upfront", Count: 1,
	}}
	err := h.priceRecommendationsFromStore(ctx, req)
	require.Error(t, err)
	ce, isClient := IsClientError(err)
	require.True(t, isClient)
	assert.Equal(t, 409, ce.code)
}

// No-ID path: the request's stated Details AZ must agree with the matched row,
// whether the top-level AZ is empty (no match) or names another AZ.
func TestPriceRecommendationsFromStore_NoIDDetailsAZMismatchRefused(t *testing.T) {
	for name, topLevel := range map[string]string{"empty top-level": "", "other top-level": "us-east-1b"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			expectStoredRecs(store, zonalEC2Rec("us-east-1a", 100), zonalEC2Rec("us-east-1b", 900))
			h := &Handler{config: store}
			req := []config.RecommendationRecord{{
				Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
				Term: 1, Payment: "all-upfront", Count: 1,
				AvailabilityZone: topLevel, Details: zonalEC2Details("us-east-1a"),
			}}
			err := h.priceRecommendationsFromStore(ctx, req)
			require.Error(t, err)
			ce, isClient := IsClientError(err)
			require.True(t, isClient)
			assert.Equal(t, 409, ce.code)
		})
	}
}

func TestDescribeRec_IncludesAvailabilityZone(t *testing.T) {
	r := zonalEC2Rec("us-east-1a", 1)
	assert.Contains(t, describeRec(&r), "az=us-east-1a")
	r.AvailabilityZone = ""
	assert.NotContains(t, describeRec(&r), "az=")
}
