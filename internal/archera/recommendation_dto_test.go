package archera

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/stretchr/testify/require"
)

func buildFixture(t *testing.T, c *insurance.Comparison, items []insurance.LineItem) (*RecommendationComparisonDTO, error) {
	t.Helper()
	record := storedMatchFixture()
	rec, err := ProjectRecommendation(&record)
	require.NoError(t, err)
	return BuildRecommendationComparison("requested-row", matchPlan, rec, c, items)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestRecommendationDTOMapsEveryFieldFromDistinctValues(t *testing.T) {
	c, items := contractMatchFixture(t)
	cur := &c.Rows[0].Current
	cur.SelectedAmount = mustRat(t, "0.50000000000000000001")
	cur.Monthly = insurance.Financials{CommitmentCostTotal: big.NewRat(101, 1), CloudProviderCost: big.NewRat(102, 1),
		Premium: big.NewRat(103, 1), GrossSavings: big.NewRat(104, 1), NetSavings: big.NewRat(105, 1), CoveredOnDemandCost: big.NewRat(106, 1)}
	cur.UpfrontCost = big.NewRat(107, 1)
	cand := &c.Rows[0].Candidates[0]
	cand.Monthly = insurance.Financials{CommitmentCostTotal: big.NewRat(201, 1), CloudProviderCost: big.NewRat(202, 1),
		Premium: big.NewRat(203, 1), GrossSavings: big.NewRat(204, 1), NetSavings: big.NewRat(205, 1), CoveredOnDemandCost: big.NewRat(206, 1)}
	cand.UpfrontCost = big.NewRat(207, 1)
	lease := "synthetic-lease"
	cand.LeaseMenuItemID = &lease
	c.Current.UpfrontCost = big.NewRat(987654321, 1)
	beforeC, beforeItems := mustMarshal(t, c), mustMarshal(t, items)

	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, "requested-row", got.RecommendationID)
	require.Equal(t, ResourceAssociated, got.Status)
	require.Empty(t, got.Reason)
	require.Equal(t, ReasonStoredDecimalOnly, got.AmountProvenance)
	require.Equal(t, ReasonUnavailableSourcePrecision, got.EconomicStatus)
	require.Equal(t, "1970-01-01T00:00:01Z", got.FetchedAt)
	require.Nil(t, got.Currency)
	require.Nil(t, got.ComparableDelta)
	require.Equal(t, currencyNote, got.CurrencyNote)
	require.True(t, got.PremiumIncluded)
	require.NotEmpty(t, got.NonGatingDisclosure)
	require.NotEmpty(t, got.SponsorshipDisclosure)

	want := RecommendationOfferDTO{OfferID: matchOffer, Provider: "aws", CommitmentType: "savings_plan",
		HourlySelectedCommitment: ptr("0.50000000000000000001"), NativeDurationSeconds: ptr("31536000"),
		NativePlanType: ptr("EC2Instance"), NativeInstanceFamily: ptr("m5"), EffectiveContractTerm: ptr("one_year_gris"),
		PaymentOption: ptr("no_upfront"), LeaseAttached: false,
		Monthly: FinancialsDTO{CommitmentCostTotal: ptr("101"), CloudProviderCost: ptr("102"), Premium: ptr("103"),
			GrossSavings: ptr("104"), NetSavings: ptr("105"), CoveredOnDemandCost: ptr("106")},
		UpfrontCost: ptr("107")}
	require.Equal(t, want, *got.Current)

	require.Len(t, got.Candidates, 2)
	alt := got.Candidates[0]
	require.Equal(t, CandidateVendorAlternative, alt.Status)
	require.Equal(t, CandidateUnavailable, alt.NativeIdentityStatus)
	wantAlt := RecommendationOfferDTO{OfferID: "faadf00d-ffff-4aba-8ada-abcdefabcdef", Provider: "aws", CommitmentType: "savings_plan",
		HourlySelectedCommitment: ptr("0.50000000000000000001"), EffectiveContractTerm: ptr("one_year_gris"),
		PaymentOption: ptr("all_upfront"), LeaseAttached: true,
		Monthly: FinancialsDTO{CommitmentCostTotal: ptr("201"), CloudProviderCost: ptr("202"), Premium: ptr("203"),
			GrossSavings: ptr("204"), NetSavings: ptr("205"), CoveredOnDemandCost: ptr("206")},
		UpfrontCost: ptr("207")}
	require.Equal(t, wantAlt, *alt.Figures)
	require.Nil(t, alt.Figures.NativeDurationSeconds)
	require.Nil(t, alt.Figures.NativePlanType)
	require.Nil(t, alt.Figures.NativeInstanceFamily)

	wire := string(mustMarshal(t, got))
	for _, leak := range []string{"Source-account-a", "fixture-org", "987654321", "hypotheticals", "delta_vs_current", "provider-offer-a", "guarantee"} {
		require.NotContains(t, wire, leak)
	}
	require.Equal(t, beforeC, mustMarshal(t, c))
	require.Equal(t, beforeItems, mustMarshal(t, items))
}

func TestRecommendationDTOKeepsZeroDistinctFromNull(t *testing.T) {
	c, items := contractMatchFixture(t)
	c.Rows[0].Current.UpfrontCost = new(big.Rat)
	c.Rows[0].Current.Monthly = insurance.Financials{Premium: new(big.Rat)}
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, "0", *got.Current.UpfrontCost)
	require.Equal(t, "0", *got.Current.Monthly.Premium)
	require.Nil(t, got.Current.Monthly.NetSavings)
}

func TestRecommendationDTOKeepsVeryPreciseDecimalsExact(t *testing.T) {
	c, items := contractMatchFixture(t)
	precise := mustRat(t, "123456789012345678.123456789012345678901234567890")
	c.Rows[0].Current.UpfrontCost = precise
	c.Rows[0].Current.Monthly.NetSavings = mustRat(t, "-0.000000000000000000000001")
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, "123456789012345678.12345678901234567890123456789", *got.Current.UpfrontCost)
	require.Equal(t, "-0.000000000000000000000001", *got.Current.Monthly.NetSavings)
}

func TestRecommendationDTOUnmatchedCarriesNoFigures(t *testing.T) {
	for name, change := range map[string]func(*RecommendationIdentity, *insurance.Comparison, *[]insurance.LineItem){
		"source": func(r *RecommendationIdentity, _ *insurance.Comparison, _ *[]insurance.LineItem) {
			r.SourceAccountID = "other"
		},
		"ambiguous": func(_ *RecommendationIdentity, c *insurance.Comparison, i *[]insurance.LineItem) {
			dup := (*i)[0]
			dup.ID = "22222222-2222-4222-8222-222222222222"
			*i = append(*i, dup)
			row := c.Rows[0]
			row.LineItemID = dup.ID
			c.Rows = append(c.Rows, row)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, items := contractMatchFixture(t)
			c.Rows[0].Current.UpfrontCost = big.NewRat(1, 3)
			record := storedMatchFixture()
			rec, err := ProjectRecommendation(&record)
			require.NoError(t, err)
			change(rec, c, &items)
			got, err := BuildRecommendationComparison("requested-row", matchPlan, rec, c, items)
			require.NoError(t, err)
			require.Equal(t, MatchUnavailable, got.Status)
			require.NotEmpty(t, got.Reason)
			require.Empty(t, got.AmountProvenance)
			require.Equal(t, ReasonIdentityUnproven, got.EconomicStatus)
			require.Nil(t, got.Current)
			require.Empty(t, got.Candidates)
			require.Nil(t, got.Currency)
			require.Nil(t, got.ComparableDelta)
			require.NotContains(t, string(mustMarshal(t, got)), "faadf00d")
		})
	}
}

func TestRecommendationDTOCandidateMismatchAndUnrepresentable(t *testing.T) {
	c, items := contractMatchFixture(t)
	c.Rows[0].Candidates[0].SelectedAmount = big.NewRat(1, 2)
	c.Rows[0].Candidates[0].UpfrontCost = big.NewRat(1, 3)
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, got.Status)
	require.Nil(t, got.Candidates[0].Figures)
	require.Equal(t, CandidateUnavailable, got.Candidates[0].Status)
	require.Equal(t, ReasonCandidateBasisUnproven, got.Candidates[0].Reason)
	require.NotNil(t, got.Candidates[1].Figures)

	for name, fn := range map[string]func(*insurance.Comparison){
		"current":   func(c *insurance.Comparison) { c.Rows[0].Current.UpfrontCost = big.NewRat(1, 3) },
		"candidate": func(c *insurance.Comparison) { c.Rows[0].Candidates[1].Monthly.Premium = big.NewRat(1, 3) },
	} {
		t.Run(name, func(t *testing.T) {
			sub, subItems := contractMatchFixture(t)
			fn(sub)
			subGot, subErr := buildFixture(t, sub, subItems)
			require.ErrorIs(t, subErr, ErrUnrepresentable)
			require.Nil(t, subGot)
		})
	}
	_, err = buildFixture(t, nil, items)
	require.Error(t, err)
}

func TestRecommendationDTOMissingRowIsAnError(t *testing.T) {
	c, items := contractMatchFixture(t)
	record := storedMatchFixture()
	rec, err := ProjectRecommendation(&record)
	require.NoError(t, err)
	rows, ids, err := ProjectContract(c, items)
	require.NoError(t, err)
	match, err := MatchRecommendation(matchPlan, rec, rows, ids)
	require.NoError(t, err)
	c.Rows = nil
	m := &mapper{}
	require.ErrorIs(t, m.fillMatched(&RecommendationComparisonDTO{}, &match, c, items), errRecommendationRowMissing)
}

func TestRecommendationDTONeverListsCurrentOfferAsAlternative(t *testing.T) {
	c, items := contractMatchFixture(t)
	row := &c.Rows[0]
	currentCopy := row.Current
	currentCopy.UpfrontCost = big.NewRat(99, 1)
	row.Candidates[0].UpfrontCost = big.NewRat(31, 1)
	row.Candidates[1].SelectedAmount = big.NewRat(1, 2)
	row.Candidates = append([]insurance.OfferEntry{currentCopy}, row.Candidates...)
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Len(t, got.Candidates, 2)
	require.Equal(t, "31", *got.Candidates[0].Figures.UpfrontCost)
	require.Nil(t, got.Candidates[1].Figures)
	require.Equal(t, ReasonCandidateBasisUnproven, got.Candidates[1].Reason)
}

func TestRecommendationDTOIgnoresUnrelatedRows(t *testing.T) {
	c, items := contractMatchFixture(t)
	unrelated := c.Rows[0]
	unrelated.LineItemID = "22222222-2222-4222-8222-222222222222"
	unrelated.Current.UpfrontCost = big.NewRat(1, 3)
	c.Rows = append(c.Rows, unrelated)
	other := items[0]
	other.ID = unrelated.LineItemID
	src := "another-source"
	other.AccountID = &src
	items = append(items, other)
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, got.Status)
	require.Len(t, got.Candidates, 2)
	require.NotContains(t, string(mustMarshal(t, got)), "another-source")
}

func TestRecommendationDTOSanitizesVendorStrings(t *testing.T) {
	c, items := contractMatchFixture(t)
	hostile := "one_year\u202e\n<script>" + strings.Repeat("界", 100)
	c.Rows[0].Current.ContractTerm = &hostile
	c.Rows[0].Candidates[0].OfferID = hostile
	c.Rows[0].Candidates[0].CommitmentType = hostile
	got, err := buildFixture(t, c, items)
	require.NoError(t, err)
	require.Equal(t, CleanString(hostile), *got.Current.EffectiveContractTerm)
	require.Equal(t, CleanString(hostile), got.Candidates[0].OfferID)
	require.LessOrEqual(t, len(got.Candidates[0].OfferID), maxVendorStrLen)
	require.NotContains(t, got.Candidates[0].OfferID, "\u202e")
	require.Equal(t, hostile, c.Rows[0].Candidates[0].OfferID)
}

func ptr(s string) *string { return &s }

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	require.True(t, ok)
	return r
}
