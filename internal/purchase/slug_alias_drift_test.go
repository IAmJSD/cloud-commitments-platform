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

// TestMapServiceSlugAcceptsEveryCanonicalReservationSlug pins the canonical
// slugs, including Azure Cosmos DB's "nosql" (#613). The Settings page writes
// service configs and overrides under these ids, and its test reads this
// table, so a slug missing here shows up there as a slug the backend rejects.
func TestMapServiceSlugAcceptsEveryCanonicalReservationSlug(t *testing.T) {
	for _, want := range []common.ServiceType{
		common.ServiceCompute, common.ServiceRelationalDB, common.ServiceNoSQL,
		common.ServiceCache, common.ServiceSearch, common.ServiceDataWarehouse,
	} {
		got, ok := mapServiceSlug(string(want))
		require.True(t, ok, "mapServiceSlug rejects canonical slug %q", want)
		assert.Equal(t, want, got)
	}
}
