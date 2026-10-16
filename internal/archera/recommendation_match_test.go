package archera

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	matchPlan  = "baadf00d-ffff-4aba-8ada-abcdefabcdef"
	matchItem  = "caadf00d-ffff-4aba-8ada-abcdefabcdef"
	matchOffer = "daadf00d-ffff-4aba-8ada-abcdefabcdef"
)

func matchFixture() (*RecommendationIdentity, []ComparisonIdentity, []LineItemIdentity) {
	r := &RecommendationIdentity{
		Provider: "aws", SourceAccountID: "source-account-a", Service: "savings-plans-ec2instance",
		PlanType: "EC2Instance", OfferingID: "provider-offer-a", Region: "us-east-1",
		InstanceFamily: "m5", Payment: "no_upfront", NativeDurationSeconds: 31536000,
		HourlyCommitment: big.NewRat(1, 2),
	}
	region := r.Region
	row := ComparisonIdentity{Region: &region, IsCurrent: true,
		LineItemID: matchItem, CurrentOfferID: matchOffer, Provider: "aws",
		CurrentType: "savings_plan", CurrentPayment: "no_upfront", CurrentSelectedAmount: big.NewRat(1, 2),
	}
	item := LineItemIdentity{
		ID: matchItem, PlanID: matchPlan, OfferID: matchOffer, Provider: "aws",
		SourceAccountID: r.SourceAccountID, ProviderOfferingID: r.OfferingID, Type: "savings_plan",
		PlanType: r.PlanType, Region: r.Region, InstanceFamily: r.InstanceFamily, Payment: r.Payment,
		DurationSeconds: big.NewRat(31536000, 1), SelectedCommitment: big.NewRat(1, 2),
	}
	return r, []ComparisonIdentity{row}, []LineItemIdentity{item}
}

func TestMatchRecommendationAssociatesOnlyProvenResourceIdentity(t *testing.T) {
	rec, rows, items := matchFixture()
	items[0].ID = strings.ToUpper(matchItem)
	items[0].PlanID = strings.ToUpper(matchPlan)
	items[0].OfferID = strings.ToUpper(matchOffer)
	before, marshalErr := json.Marshal([]any{rec, rows, items})
	require.NoError(t, marshalErr)
	result, err := MatchRecommendation(matchPlan, rec, rows, items)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, result.Status)
	require.Equal(t, matchItem, result.LineItemID)
	require.Equal(t, "stored_decimal_only", result.AmountProvenance)
	require.Equal(t, "unavailable_source_precision", result.EconomicStatus)
	require.Equal(t, "1/2", rec.HourlyCommitment.RatString())
	require.Equal(t, "1/2", items[0].SelectedCommitment.RatString())
	after, marshalErr := json.Marshal([]any{rec, rows, items})
	require.NoError(t, marshalErr)
	require.Equal(t, before, after)
	rec.NativeDurationSeconds = 94608000
	items[0].DurationSeconds = big.NewRat(94608000, 1)
	result, err = MatchRecommendation(matchPlan, rec, rows, items)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, result.Status)
}

func TestMatchRecommendationRefusesUnprovenAssociation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*RecommendationIdentity, *ComparisonIdentity, *LineItemIdentity)
	}{
		{"unusable_source", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.SourceAccountID = "source\naccount"
			i.SourceAccountID = r.SourceAccountID
		}},
		{"unknown_source", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.SourceAccountID = ""
			i.SourceAccountID = ""
		}},
		{"collector_is_not_source", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.SourceAccountID = "collector-account"
		}},
		{"source_case", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.SourceAccountID = "SOURCE-account-a"
		}},
		{"provider", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) { i.Provider = "gcp" }},
		{"offering", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.ProviderOfferingID = "other-offer"
		}},
		{"missing_offering", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.OfferingID = ""
			i.ProviderOfferingID = ""
		}},
		{"unsupported_service", func(r *RecommendationIdentity, _ *ComparisonIdentity, _ *LineItemIdentity) {
			r.Service = "savings-plans-compute"
		}},
		{"invented_type", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.Type = "aws/savingsplan/EC2Instance"
		}},
		{"plan_type", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) { i.PlanType = "Compute" }},
		{"family", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) { i.InstanceFamily = "m6" }},
		{"missing_details_region", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.Region = ""
			i.Region = ""
		}},
		{"contradictory_top_region", func(r *RecommendationIdentity, _ *ComparisonIdentity, _ *LineItemIdentity) {
			r.TopLevelRegion = "us-west-2"
		}},
		{"native_duration", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.DurationSeconds = big.NewRat(94608000, 1)
		}},
		{"payment", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) { i.Payment = "all_upfront" }},
		{"amount_mismatch", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.SelectedCommitment = big.NewRat(1, 1)
		}},
		{"missing_hourly", func(r *RecommendationIdentity, _ *ComparisonIdentity, _ *LineItemIdentity) { r.HourlyCommitment = nil }},
		{"zero_hourly", func(r *RecommendationIdentity, _ *ComparisonIdentity, _ *LineItemIdentity) {
			r.HourlyCommitment = new(big.Rat)
		}},
		{"current_amount", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) {
			c.CurrentSelectedAmount = big.NewRat(2, 1)
		}},
		{"offering_case", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.ProviderOfferingID = "PROVIDER-offer-a"
		}},
		{"missing_family", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.InstanceFamily = ""
			i.InstanceFamily = ""
		}},
		{"unsupported_native_term", func(r *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			r.NativeDurationSeconds = 123
			i.DurationSeconds = big.NewRat(123, 1)
		}},
		{"current_provider", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) { c.Provider = "gcp" }},
		{"current_unit_type", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) { c.CurrentType = "ri" }},
		{"current_flag", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) { c.IsCurrent = false }},
		{"current_region", func(_ *RecommendationIdentity, c *ComparisonIdentity, i *LineItemIdentity) {
			c.Region = &i.InstanceFamily
		}},
		{"missing_current_region", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) { c.Region = nil }},
		{"line_item_region", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) { i.Region = "eu-west-1" }},
		{"current_payment", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) {
			c.CurrentPayment = "all_upfront"
		}},
		{"other_plan", func(_ *RecommendationIdentity, _ *ComparisonIdentity, i *LineItemIdentity) {
			i.PlanID = "faadf00d-ffff-4aba-8ada-abcdefabcdef"
		}},
		{"non_aws_recommendation", func(r *RecommendationIdentity, c *ComparisonIdentity, i *LineItemIdentity) {
			r.Provider, c.Provider, i.Provider = "gcp", "gcp", "gcp"
		}},
		{"invalid_payment", func(r *RecommendationIdentity, c *ComparisonIdentity, i *LineItemIdentity) {
			r.Payment, c.CurrentPayment, i.Payment = "monthly", "monthly", "monthly"
		}},
		{"current_offer", func(_ *RecommendationIdentity, c *ComparisonIdentity, _ *LineItemIdentity) {
			c.CurrentOfferID = "faadf00d-ffff-4aba-8ada-abcdefabcdef"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, rows, items := matchFixture()
			tt.change(r, &rows[0], &items[0])
			got, err := MatchRecommendation(matchPlan, r, rows, items)
			require.NoError(t, err)
			require.Equal(t, MatchUnavailable, got.Status)
			require.Empty(t, got.LineItemID)
		})
	}
}

func TestMatchRecommendationRejectsAmbiguousVendorRows(t *testing.T) {
	r, rows, items := matchFixture()
	rows = append(rows, rows[0])
	_, err := MatchRecommendation(matchPlan, r, rows, items)
	require.Error(t, err)
	r, rows, items = matchFixture()
	items = append(items, items[0])
	items[1].ID = "eaadf00d-ffff-4aba-8ada-abcdefabcdef"
	rows = append(rows, rows[0])
	rows[1].LineItemID = items[1].ID
	got, err := MatchRecommendation(matchPlan, r, rows, items)
	require.NoError(t, err)
	require.Equal(t, MatchUnavailable, got.Status)
	require.Empty(t, got.LineItemID)
}

func TestMatchRecommendationRejectsEqualMalformedUUIDReferences(t *testing.T) {
	for _, bad := range []string{"", "not-a-uuid", strings.ReplaceAll(matchItem, "-", ""), "zzzzzzzz-zzzz-4zzz-8zzz-zzzzzzzzzzzz"} {
		for _, field := range []string{"plan", "item", "offer"} {
			t.Run(field+"_"+bad, func(t *testing.T) {
				r, rows, items := matchFixture()
				plan := matchPlan
				switch field {
				case "plan":
					plan, items[0].PlanID = bad, bad
				case "item":
					rows[0].LineItemID, items[0].ID = bad, bad
				case "offer":
					rows[0].CurrentOfferID, items[0].OfferID = bad, bad
				}
				got, err := MatchRecommendation(plan, r, rows, items)
				require.NoError(t, err)
				require.Equal(t, MatchUnavailable, got.Status)
			})
		}
	}
}
