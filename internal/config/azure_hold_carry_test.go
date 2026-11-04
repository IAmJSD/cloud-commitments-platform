package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func holdOf(ids ...string) *RecommendationsCollectionPartial {
	return &RecommendationsCollectionPartial{Providers: []PartialProviderSweep{{
		Provider: "azure", HeldAccountIDs: ids, Reasons: map[string]int{PartialReasonPricingUnavailable: 1},
	}}}
}

func azureCollect(id *string) []SuccessfulCollect {
	return []SuccessfulCollect{{Provider: "azure", CloudAccountID: id}}
}

func TestCarryAzureHold(t *testing.T) {
	a, b, c := "a", "b", "c"
	stored := map[string]bool{"a": true, "b": true, "c": true}
	ambient := map[string]bool{ambientAccountKey: true}
	for name, tc := range map[string]struct {
		prev     *RecommendationsCollectionPartial
		collects []SuccessfulCollect
		partial  *RecommendationsCollectionPartial
		stored   map[string]bool
		want     []string
		wantHold bool
	}{
		"no previous hold":                   {nil, nil, nil, stored, nil, false},
		"nothing re-priced":                  {holdOf("a", "b"), nil, nil, stored, []string{"a", "b"}, true},
		"one re-priced":                      {holdOf("a", "b"), azureCollect(&a), nil, stored, []string{"b"}, true},
		"both re-priced":                     {holdOf("a", "b"), append(azureCollect(&a), azureCollect(&b)...), nil, stored, nil, false},
		"other provider ignored":             {holdOf("a"), []SuccessfulCollect{{Provider: "gcp", CloudAccountID: &a}}, nil, stored, []string{"a"}, true},
		"new hold is merged":                 {holdOf("a", "b"), nil, holdOf("c"), stored, []string{"a", "b", "c"}, true},
		"re-held account dedupes":            {holdOf("a", "b"), nil, holdOf("a"), stored, []string{"a", "b"}, true},
		"account without rows drops":         {holdOf("a", "b"), nil, nil, map[string]bool{"b": true}, []string{"b"}, true},
		"all rows gone drops":                {holdOf("a"), nil, nil, map[string]bool{"c": true}, nil, false},
		"ambient carries while rows exist":   {holdOf(), nil, nil, ambient, []string{ambientAccountKey}, true},
		"ambient survives account rows too":  {holdOf(), azureCollect(&c), nil, map[string]bool{ambientAccountKey: true, "c": true}, []string{ambientAccountKey}, true},
		"ambient re-priced":                  {holdOf(), azureCollect(nil), nil, ambient, nil, false},
		"ambient drops with no ambient rows": {holdOf(), nil, nil, stored, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := carryAzureHold(tc.prev, tc.partial, tc.collects, tc.stored)
			if !tc.wantHold {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Len(t, got.Providers, 1)
			assert.ElementsMatch(t, tc.want, got.Providers[0].HeldAccountIDs)
			assert.Positive(t, got.Providers[0].Reasons[PartialReasonPricingUnavailable])
		})
	}
}

// An ambient hold merged with a named account's hold must stay an ambient hold
// after the named account re-prices: the stale ambient rows are still there.
func TestCarryAzureHold_AmbientSurvivesNamedReprice(t *testing.T) {
	c := "c"
	stored := map[string]bool{ambientAccountKey: true, "c": true}

	// Cycle 1: the ambient hold carries while this cycle holds account c.
	merged := carryAzureHold(holdOf(), holdOf("c"), nil, stored)
	require.NotNil(t, merged)

	// Cycle 2: c re-prices, the ambient identity does not.
	got := carryAzureHold(merged, nil, azureCollect(&c), stored)
	require.NotNil(t, got, "ambient rows still exist, so the hold must stay")
	assert.Contains(t, got.Providers[0].HeldAccountIDs, ambientAccountKey)

	// Cycle 3: the ambient identity re-prices, which ends the hold.
	assert.Nil(t, carryAzureHold(got, nil, azureCollect(nil), stored))
}

// An ambient pricing outage in this cycle plus a carried named hold keeps both.
func TestCarryAzureHold_ThisCycleAmbientKeptWithCarriedNamed(t *testing.T) {
	stored := map[string]bool{ambientAccountKey: true, "a": true}
	got := carryAzureHold(holdOf("a"), holdOf(), nil, stored)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"a", ambientAccountKey}, got.Providers[0].HeldAccountIDs)
}

// Carried accounts add to the incomplete count instead of being absorbed into a
// larger count that came from other failures this cycle.
func TestCarryAzureHold_IncompleteAccountsCountsEachCarriedAccount(t *testing.T) {
	stored := map[string]bool{"a": true, "b": true, "x": true, ambientAccountKey: true}
	for name, tc := range map[string]struct {
		prev, partial *RecommendationsCollectionPartial
		want          int
	}{
		"carried on top of other failures": {holdOf("a", "b"), withIncomplete(holdOf("x"), 3), 5},
		"own ambient outage counted once":  {holdOf("a"), withIncomplete(holdOf(), 1), 2},
		"nothing this cycle":               {holdOf("a", "b"), nil, 2},
		"ambient carried":                  {holdOf(), nil, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := carryAzureHold(tc.prev, tc.partial, nil, stored)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Providers[0].IncompleteAccounts)
		})
	}
}

func withIncomplete(p *RecommendationsCollectionPartial, n int) *RecommendationsCollectionPartial {
	p.Providers[0].IncompleteAccounts = n
	return p
}
