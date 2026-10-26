package purchase

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacyAWSSlugsMatchSuppressionAliases guards the pair of alias tables
// (config.awsLegacyServiceSlugs and legacyAWSServiceSlugs) against drifting
// apart, which would let a suppression miss a rec spelled the other way (#1420).
func TestLegacyAWSSlugsMatchSuppressionAliases(t *testing.T) {
	canonicalFamily := map[string]common.ServiceType{
		"ec2":         common.ServiceCompute,
		"rds":         common.ServiceRelationalDB,
		"elasticache": common.ServiceCache,
		"opensearch":  common.ServiceSearch,
		"redshift":    common.ServiceDataWarehouse,
	}
	// Deleting an entry from legacyAWSServiceSlugs must not pass silently.
	for key := range canonicalFamily {
		require.Contains(t, legacyAWSServiceSlugs, key, "legacy slug %q was removed from legacyAWSServiceSlugs", key)
	}
	require.Contains(t, legacyAWSServiceSlugs, string(common.ServiceMemoryDB), "legacy slug memorydb was removed from legacyAWSServiceSlugs")
	for key := range legacyAWSServiceSlugs {
		t.Run(key, func(t *testing.T) {
			canonical := config.CanonicalSuppressionService("aws", key)
			if key == string(common.ServiceMemoryDB) {
				// Documented exception: no canonical slug, passes through.
				assert.Equal(t, key, canonical)
				return
			}
			want, ok := canonicalFamily[key]
			require.True(t, ok, "legacy key %q has no expected canonical family; add it to this test and config.awsLegacyServiceSlugs", key)
			assert.NotEqual(t, key, canonical, "legacy key %q is missing from config.awsLegacyServiceSlugs", key)
			got, ok := mapServiceSlug(canonical)
			require.True(t, ok, "mapServiceSlug rejects canonical slug %q", canonical)
			assert.Equal(t, want, got)
		})
	}
}

// TestSavingsPlansSpellingsMatchSuppressionAliases checks that every per-type
// Savings Plans spelling the purchase manager accepts canonicalises, for
// suppressions, to the slug it buys, so a suppression stored under the
// dash-free spelling still hides the hyphenated rec (#1422). The umbrella
// spellings must stay umbrellas: folding one into a plan type would let one
// umbrella suppression hide every plan type's recs.
func TestSavingsPlansSpellingsMatchSuppressionAliases(t *testing.T) {
	for _, pt := range common.SavingsPlansPlanTypes() {
		require.Contains(t, savingsPlansSlugs, string(pt), "per-type slug %q was removed from savingsPlansSlugs", pt)
	}
	for key, want := range savingsPlansSlugs {
		t.Run(key, func(t *testing.T) {
			canonical := config.CanonicalSuppressionService("aws", key)
			if want == common.ServiceSavingsPlansAll {
				assert.Equal(t, key, canonical, "umbrella spelling %q must pass through unchanged", key)
				return
			}
			assert.Equal(t, string(want), canonical, "spelling %q is missing from config.awsSavingsPlansSpellings", key)
		})
	}
}
