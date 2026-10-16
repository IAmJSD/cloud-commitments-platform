package archera

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func candidateFixture() CandidateIdentity {
	return CandidateIdentity{OfferID: "faadf00d-ffff-4aba-8ada-abcdefabcdef", Provider: "aws",
		Type: "savings_plan", Payment: "all_upfront", SelectedAmount: big.NewRat(1, 2)}
}

func TestMatchRecommendationPreservesVendorAlternativesWithoutNativeClaims(t *testing.T) {
	r, rows, items := matchFixture()
	candidate := candidateFixture()
	rows[0].Candidates = []CandidateIdentity{candidate, candidate, candidate}
	rows[0].Candidates[1].Payment = "partial_upfront"
	rows[0].Candidates[2].OfferID = strings.ToUpper(candidate.OfferID)
	rows[0].Candidates[2].Payment = "no_upfront"
	before, err := json.Marshal([]any{r, rows, items})
	require.NoError(t, err)
	got, err := MatchRecommendation(matchPlan, r, rows, items)
	require.NoError(t, err)
	require.Equal(t, ResourceAssociated, got.Status)
	require.Len(t, got.Candidates, 3)
	for i, association := range got.Candidates {
		require.Equal(t, rows[0].Candidates[i].OfferID, association.OfferID)
		require.Equal(t, "vendor_alternative", association.Status)
		require.Empty(t, association.Reason)
		require.Equal(t, "unavailable", association.NativeIdentityStatus)
	}
	require.Equal(t, "unavailable_source_precision", got.EconomicStatus)
	after, err := json.Marshal([]any{r, rows, items})
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestMatchRecommendationDoesNotBorrowCandidateBasis(t *testing.T) {
	tests := []struct {
		name   string
		change func(*CandidateIdentity)
	}{
		{"id", func(c *CandidateIdentity) { c.OfferID = "invalid" }},
		{"invalid36", func(c *CandidateIdentity) { c.OfferID = "zzzzzzzz-zzzz-4zzz-8zzz-zzzzzzzzzzzz" }},
		{"type", func(c *CandidateIdentity) { c.Type = "ri" }},
		{"amount", func(c *CandidateIdentity) { c.SelectedAmount = big.NewRat(2, 1) }},
		{"missing_amount", func(c *CandidateIdentity) { c.SelectedAmount = nil }},
		{"zero_amount", func(c *CandidateIdentity) { c.SelectedAmount = new(big.Rat) }},
		{"negative_amount", func(c *CandidateIdentity) { c.SelectedAmount = big.NewRat(-1, 2) }},
		{"provider", func(c *CandidateIdentity) { c.Provider = "azure" }},
		{"payment", func(c *CandidateIdentity) { c.Payment = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, rows, items := matchFixture()
			c := candidateFixture()
			tt.change(&c)
			rows[0].Candidates = []CandidateIdentity{c}
			got, err := MatchRecommendation(matchPlan, r, rows, items)
			require.NoError(t, err)
			require.Equal(t, ResourceAssociated, got.Status)
			require.Len(t, got.Candidates, 1)
			require.Equal(t, "unavailable", got.Candidates[0].Status)
			require.NotEmpty(t, got.Candidates[0].Reason)
			require.Equal(t, "unavailable", got.Candidates[0].NativeIdentityStatus)
		})
	}
}

func TestMatchRecommendationOmitsCandidatesWithoutCurrentAssociation(t *testing.T) {
	r, rows, items := matchFixture()
	rows[0].Candidates = []CandidateIdentity{candidateFixture()}
	items[0].SourceAccountID = "another-source"
	got, err := MatchRecommendation(matchPlan, r, rows, items)
	require.NoError(t, err)
	require.Equal(t, MatchUnavailable, got.Status)
	require.Empty(t, got.Candidates)
}
