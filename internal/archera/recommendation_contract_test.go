package archera

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/stretchr/testify/require"
)

func contractMatchFixture(t *testing.T) (*insurance.Comparison, []insurance.LineItem) {
	t.Helper()
	item := `{"id":"` + matchItem + `","plan_id":"` + matchPlan + `","offer_id":"` + matchOffer + `","account_id":"Source-account-a","selected_quantity":7,"selected_commitment":0.50000000000000000001,"is_selected":true,"contract_term":"one_year_gris","payment_option":"No Upfront","contract_spec":{"commitment_type":"opaque-contract-type"},"offer":{"id":"` + matchOffer + `","type":"savings_plan","provider":"aws","provider_offering_id":"provider-offer-a","offering_id":"distinct-catalog-offer","duration_seconds":31536000,"region":"us-east-1","az":null,"instance_type":null,"instance_family":"m5","product_description":null,"tenancy":null,"plan_type":"EC2Instance","is_multi_az":null,"is_flexible":null,"payment_option":"No Upfront"}}`
	items, err := insurance.DecodeLineItems(strings.NewReader("["+item+"]"), matchPlan)
	require.NoError(t, err)
	current := `{"is_current":true,"offer_id":"` + matchOffer + `","selected_amount":0.50000000000000000001,"commitment_type":"savings_plan","contract_term":"one_year_gris","payment_option":"no_upfront","offer":{"provider":"aws","region":"us-east-1"},"discount_rate":0,"commitment_upfront_cost":0,"commitment_financials_monthly_rate":{},"delta_vs_current":{"monthly_net_savings":0,"upfront_cost":0,"discount_rate":0}}`
	candidate := strings.ReplaceAll(strings.Replace(current, `"is_current":true`, `"is_current":false`, 1), matchOffer, "faadf00d-ffff-4aba-8ada-abcdefabcdef")
	candidate = strings.Replace(candidate, "no_upfront", "all_upfront", 1)
	body := `{"current_totals":{"commitment_financials_monthly_rate":{},"commitment_upfront_cost":0},"hypothetical_totals":[],"data":[{"line_item_id":"` + matchItem + `","current":` + current + `,"candidates":[` + candidate + "," + candidate + `]}]}`
	comparison, err := insurance.DecodeComparison(strings.NewReader(body), "fixture-org", matchPlan, time.Unix(1, 0))
	require.NoError(t, err)
	return comparison, items
}

func TestProjectContractJoinsActualDecodedIdentity(t *testing.T) {
	comparison, items := contractMatchFixture(t)
	record := storedMatchFixture()
	rec, err := ProjectRecommendation(&record)
	require.NoError(t, err)
	rows, identities, err := ProjectContract(comparison, items)
	require.NoError(t, err)
	require.Len(t, rows[0].Candidates, 2)
	require.Equal(t, "all_upfront", rows[0].Candidates[0].Payment)
	require.Equal(t, rows[0].Candidates[0].OfferID, rows[0].Candidates[1].OfferID)
	require.Equal(t, "provider-offer-a", identities[0].ProviderOfferingID)
	require.Equal(t, "opaque-contract-type", items[0].ContractSpec.CommitmentType)
	require.Equal(t, "one_year_gris", items[0].ContractTerm)
	got, err := MatchRecommendation(matchPlan, rec, rows, identities)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, got.Status)
	require.Len(t, got.Candidates, 2)
	require.Equal(t, CandidateVendorAlternative, got.Candidates[0].Status)
	require.Equal(t, ReasonStoredDecimalOnly, got.AmountProvenance)
	require.Equal(t, ReasonUnavailableSourcePrecision, got.EconomicStatus)
	_, _, err = ProjectContract(nil, items)
	require.Error(t, err)
}

func TestProjectContractPreservesMissingOrContradictoryEvidence(t *testing.T) {
	tests := []struct {
		name   string
		change func(*insurance.Comparison, *insurance.LineItem)
	}{
		{"account", func(_ *insurance.Comparison, i *insurance.LineItem) { i.AccountID = nil }},
		{"catalog_is_not_provider_offering", func(_ *insurance.Comparison, i *insurance.LineItem) { i.Offer.ProviderOfferingID = "" }},
		{"duration", func(_ *insurance.Comparison, i *insurance.LineItem) { i.Offer.DurationSeconds = nil }},
		{"region", func(_ *insurance.Comparison, i *insurance.LineItem) { i.Offer.Region = nil }},
		{"family", func(_ *insurance.Comparison, i *insurance.LineItem) { i.Offer.InstanceFamily = nil }},
		{"plan_type", func(_ *insurance.Comparison, i *insurance.LineItem) { i.Offer.PlanType = nil }},
		{"unselected", func(_ *insurance.Comparison, i *insurance.LineItem) { i.IsSelected = false }},
		{"payment", func(_ *insurance.Comparison, i *insurance.LineItem) {
			p := insurance.PaymentAllUpfront
			i.Offer.PaymentOption = &p
		}},
		{"current_flag", func(c *insurance.Comparison, _ *insurance.LineItem) { c.Rows[0].Current.IsCurrent = false }},
		{"current_region", func(c *insurance.Comparison, _ *insurance.LineItem) { c.Rows[0].Current.Region = nil }},
		{"current_type", func(c *insurance.Comparison, _ *insurance.LineItem) { c.Rows[0].Current.CommitmentType = "ri" }},
		{"amount", func(c *insurance.Comparison, _ *insurance.LineItem) {
			c.Rows[0].Current.SelectedAmount = big.NewRat(1, 2)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, items := contractMatchFixture(t)
			tt.change(c, &items[0])
			record := storedMatchFixture()
			rec, err := ProjectRecommendation(&record)
			require.NoError(t, err)
			rows, identities, err := ProjectContract(c, items)
			require.NoError(t, err)
			got, err := MatchRecommendation(matchPlan, rec, rows, identities)
			require.NoError(t, err)
			require.Equal(t, MatchUnavailable, got.Status)
		})
	}
}

func TestProjectContractExcludesTheCurrentOfferFromAlternatives(t *testing.T) {
	comparison, items := contractMatchFixture(t)
	row := &comparison.Rows[0]
	current := row.Current
	current.IsCurrent = true
	row.Candidates = []insurance.OfferEntry{row.Candidates[0], current, row.Candidates[1]}
	rows, _, err := ProjectContract(comparison, items)
	require.NoError(t, err)
	require.Len(t, rows[0].Candidates, 2)
	for _, candidate := range rows[0].Candidates {
		require.NotEqual(t, current.OfferID, candidate.OfferID)
	}
	record := storedMatchFixture()
	rec, err := ProjectRecommendation(&record)
	require.NoError(t, err)
	_, identities, err := ProjectContract(comparison, items)
	require.NoError(t, err)
	got, err := MatchRecommendation(matchPlan, rec, rows, identities)
	require.NoError(t, err)
	require.Len(t, got.Candidates, 2)
	require.Empty(t, vendorAlternatives(nil))
}

func TestVendorAlternativesMapsEveryField(t *testing.T) {
	payment := insurance.PaymentPartialUpfront
	entries := []insurance.OfferEntry{
		{IsCurrent: true, OfferID: "current"},
		{OfferID: "offer-a", Provider: "aws", CommitmentType: "savings_plan", PaymentOption: &payment, SelectedAmount: big.NewRat(3, 4)},
		{OfferID: "offer-b", Provider: "azure", CommitmentType: "ri"},
	}
	got := vendorAlternatives(entries)
	require.Equal(t, []CandidateIdentity{
		{OfferID: "offer-a", Provider: "aws", Type: "savings_plan", Payment: "partial_upfront", SelectedAmount: big.NewRat(3, 4)},
		{OfferID: "offer-b", Provider: "azure", Type: "ri"},
	}, got)
}

func TestProjectContractCarriesPaymentOptions(t *testing.T) {
	comparison, items := contractMatchFixture(t)
	upfront, partial := insurance.PaymentAllUpfront, insurance.PaymentPartialUpfront
	items[0].PaymentOption, items[0].Offer.PaymentOption = upfront, &upfront
	comparison.Rows[0].Current.PaymentOption = &partial
	rows, identities, err := ProjectContract(comparison, items)
	require.NoError(t, err)
	require.Equal(t, "all_upfront", identities[0].Payment)
	require.Equal(t, "partial_upfront", rows[0].CurrentPayment)
}

func TestProjectContractUnselectedTwinDoesNotCreateAmbiguity(t *testing.T) {
	comparison, items := contractMatchFixture(t)
	twin := items[0]
	twin.ID = "eaadf00d-ffff-4aba-8ada-abcdefabcdef"
	twin.IsSelected = false
	items = append(items, twin)
	record := storedMatchFixture()
	rec, err := ProjectRecommendation(&record)
	require.NoError(t, err)
	_, identities, err := ProjectContract(comparison, items)
	require.NoError(t, err)
	require.Empty(t, identities[1].Type)
	id, status := SelectLineItem(rec, identities)
	require.Equal(t, SelectOne, status)
	require.Equal(t, matchItem, id)
	_, status = SelectLineItem(rec, identities[1:])
	require.Equal(t, SelectNone, status)
}
