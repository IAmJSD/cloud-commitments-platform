package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSuppressions_CreatesOneRowPerTuple(t *testing.T) {
	now := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	gp := &GlobalConfig{GracePeriodDays: map[string]int{"aws": 7, "azure": 7, "gcp": 7}}

	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 3, CloudAccountID: strPtr("acct-1")},
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 2, CloudAccountID: strPtr("acct-1")},
		{Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.t4g.micro", Engine: "postgres", Count: 1, CloudAccountID: strPtr("acct-1")},
	}, "exec-1", gp, now)

	require.Len(t, sups, 2, "duplicate 6-tuple coalesced to one row; distinct service → separate row")
	// First bucket: summed count (3 + 2).
	assert.Equal(t, 5, sups[0].SuppressedCount)
	assert.Equal(t, "t4g.nano", sups[0].ResourceType)
	// Second bucket: the RDS one with engine=postgres.
	assert.Equal(t, 1, sups[1].SuppressedCount)
	assert.Equal(t, "postgres", sups[1].Engine)
	// Expiry = now + 7 days for every row.
	assert.Equal(t, now.Add(7*24*time.Hour), sups[0].ExpiresAt)
}

func TestBuildSuppressions_SkipsProvidersWithGrace0(t *testing.T) {
	now := time.Now()
	gp := &GlobalConfig{GracePeriodDays: map[string]int{"aws": 7, "azure": 0, "gcp": 14}}

	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 1, CloudAccountID: strPtr("acct-1")},
		{Provider: "azure", Service: "compute", Region: "eastus", ResourceType: "Standard_D2d_v5", Count: 2, CloudAccountID: strPtr("acct-2")},
		{Provider: "gcp", Service: "compute", Region: "us-central1", ResourceType: "n1-standard-1", Count: 3, CloudAccountID: strPtr("acct-3")},
	}, "exec-1", gp, now)

	require.Len(t, sups, 2, "azure has grace=0 → no row; aws + gcp still produce rows")
	providers := []string{sups[0].Provider, sups[1].Provider}
	assert.Contains(t, providers, "aws")
	assert.Contains(t, providers, "gcp")
	assert.NotContains(t, providers, "azure")
}

func TestBuildSuppressions_NilAccountIDNormalised(t *testing.T) {
	now := time.Now()
	gp := &GlobalConfig{}

	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 1, CloudAccountID: nil},
	}, "exec-1", gp, now)

	require.Len(t, sups, 1)
	assert.Equal(t, "", sups[0].AccountID, "nil CloudAccountID → empty-string account_id")
}

func TestBuildSuppressions_SkipsZeroCountRecs(t *testing.T) {
	now := time.Now()
	gp := &GlobalConfig{}

	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 0, CloudAccountID: strPtr("acct-1")},
	}, "exec-1", gp, now)

	assert.Empty(t, sups, "zero-count recs don't produce suppression rows")
}

// TestBuildSuppressions_SeparatesVariants pins issue #816: recs on one 6-tuple
// that differ only by availability zone, platform or tenancy get one row each,
// carrying the rec's variant; recs with the same variant are still summed.
func TestBuildSuppressions_SeparatesVariants(t *testing.T) {
	ec2 := func(az, platform, tenancy string, count int) RecommendationRecord {
		return RecommendationRecord{
			Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
			Count: count, CloudAccountID: strPtr("acct-1"), AvailabilityZone: az,
			Details: []byte(`{"platform":"` + platform + `","tenancy":"` + tenancy + `"}`),
		}
	}
	sups := BuildSuppressions([]RecommendationRecord{
		ec2("us-east-1a", "Linux/UNIX", "default", 1),
		ec2("us-east-1a", "Linux/UNIX", "default", 2),
		ec2("us-east-1b", "Linux/UNIX", "default", 3),
		ec2("", "Linux/UNIX", "default", 4),
		ec2("us-east-1a", "Windows", "default", 5),
		ec2("us-east-1a", "Linux/UNIX", "dedicated", 6),
	}, "exec-1", &GlobalConfig{}, time.Now())

	require.Len(t, sups, 5)
	want := []struct {
		variant SuppressionVariant
		count   int
	}{
		{SuppressionVariant{"us-east-1a", "Linux/UNIX", "default"}, 3},
		{SuppressionVariant{"us-east-1b", "Linux/UNIX", "default"}, 3},
		{SuppressionVariant{"", "Linux/UNIX", "default"}, 4},
		{SuppressionVariant{"us-east-1a", "Windows", "default"}, 5},
		{SuppressionVariant{"us-east-1a", "Linux/UNIX", "dedicated"}, 6},
	}
	for i, w := range want {
		require.NotNil(t, sups[i].Variant, "row %d", i)
		assert.Equal(t, w.variant, *sups[i].Variant, "row %d", i)
		assert.Equal(t, w.count, sups[i].SuppressedCount, "row %d", i)
	}
}

// TestBuildSuppressions_NonComputeVariantIsKnownEmpty: a rec without platform
// or tenancy (non-EC2, no Details) gets an empty but known variant, not a
// legacy wildcard.
func TestBuildSuppressions_NonComputeVariantIsKnownEmpty(t *testing.T) {
	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.t4g.micro", Engine: "postgres", Count: 1},
	}, "exec-1", &GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	assert.Equal(t, &SuppressionVariant{}, sups[0].Variant)
}

// TestBuildSuppressions_UnreadableDetailsFallBackToWildcard: a rec whose
// Details cannot be read gets no variant, so its row suppresses the whole
// 6-tuple (pre-#816 behavior) instead of nothing.
func TestBuildSuppressions_UnreadableDetailsFallBackToWildcard(t *testing.T) {
	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 2,
			AvailabilityZone: "us-east-1a", Details: []byte(`{"platform":42}`)},
		{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 1,
			AvailabilityZone: "us-east-1a", Details: []byte(`{"platform":"Linux/UNIX","tenancy":"default"}`)},
	}, "exec-1", &GlobalConfig{}, time.Now())
	require.Len(t, sups, 2)
	assert.Nil(t, sups[0].Variant)
	assert.Equal(t, 2, sups[0].SuppressedCount)
	require.NotNil(t, sups[1].Variant)
	assert.Equal(t, 1, sups[1].SuppressedCount)
}

// TestPurchaseSuppression_AppliesTo pins the match rule shared by the
// scheduler: exact variant match, legacy rows and unknown rec variants are
// wildcards.
func TestPurchaseSuppression_AppliesTo(t *testing.T) {
	v := SuppressionVariant{AvailabilityZone: "us-east-1a", Platform: "Linux/UNIX", Tenancy: "default"}
	scoped := PurchaseSuppression{Variant: &v}
	legacy := PurchaseSuppression{}

	assert.True(t, scoped.AppliesTo(&SuppressionVariant{"us-east-1a", "Linux/UNIX", "default"}))
	assert.False(t, scoped.AppliesTo(&SuppressionVariant{"us-east-1b", "Linux/UNIX", "default"}))
	assert.False(t, scoped.AppliesTo(&SuppressionVariant{"", "Linux/UNIX", "default"}))
	assert.False(t, scoped.AppliesTo(&SuppressionVariant{"us-east-1a", "Windows", "default"}))
	assert.False(t, scoped.AppliesTo(&SuppressionVariant{"us-east-1a", "Linux/UNIX", "dedicated"}))
	assert.True(t, scoped.AppliesTo(nil), "an unreadable rec variant matches every row")
	assert.True(t, legacy.AppliesTo(&SuppressionVariant{"us-east-1b", "Windows", "dedicated"}), "a legacy row is a wildcard")
	assert.True(t, legacy.AppliesTo(nil))
}

// TestBuildSuppressions_LegacyEC2WithoutDetailsIsWildcard: an EC2 rec saved
// before Details existed has an unknown platform and tenancy, so it must
// suppress every variant (issue #1232). Non-EC2 and fully detailed recs keep
// their known variant.
func TestBuildSuppressions_LegacyEC2WithoutDetailsIsWildcard(t *testing.T) {
	base := RecommendationRecord{Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 1, AvailabilityZone: "us-east-1a"}
	for name, details := range map[string][]byte{"nil": nil, "empty": {}, "null": []byte(`null`), "no keys": []byte(`{}`)} {
		t.Run(name, func(t *testing.T) {
			rec := base
			rec.Details = details
			sups := BuildSuppressions([]RecommendationRecord{rec}, "exec-1", &GlobalConfig{}, time.Now())
			require.Len(t, sups, 1)
			assert.Nil(t, sups[0].Variant)
			assert.True(t, sups[0].AppliesTo(&SuppressionVariant{"us-east-1a", "Linux/UNIX", "default"}))
		})
	}

	full := base
	full.Details = []byte(`{"platform":"Linux/UNIX","tenancy":"default"}`)
	sups := BuildSuppressions([]RecommendationRecord{full}, "exec-1", &GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	assert.Equal(t, &SuppressionVariant{"us-east-1a", "Linux/UNIX", "default"}, sups[0].Variant)
}

// TestBuildSuppressions_ComputeSlugEC2: AWS EC2 recs under the canonical
// "compute" slug follow the same wildcard rule as "ec2"; a rec with only a
// platform or only a tenancy keeps its narrow variant.
func TestBuildSuppressions_ComputeSlugEC2(t *testing.T) {
	base := RecommendationRecord{Provider: "aws", Service: "compute", Region: "us-east-1", ResourceType: "m5.large", Count: 1, AvailabilityZone: "us-east-1a"}
	sups := BuildSuppressions([]RecommendationRecord{base}, "exec-1", &GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	assert.Nil(t, sups[0].Variant)
	assert.True(t, sups[0].AppliesTo(&SuppressionVariant{"us-east-1a", "Linux/UNIX", "default"}))

	for name, tc := range map[string]struct {
		details []byte
		want    SuppressionVariant
	}{
		"platform only": {[]byte(`{"platform":"Linux/UNIX"}`), SuppressionVariant{"us-east-1a", "Linux/UNIX", ""}},
		"tenancy only":  {[]byte(`{"tenancy":"default"}`), SuppressionVariant{"us-east-1a", "", "default"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := base
			rec.Details = tc.details
			sups := BuildSuppressions([]RecommendationRecord{rec}, "exec-1", &GlobalConfig{}, time.Now())
			require.Len(t, sups, 1)
			assert.Equal(t, &tc.want, sups[0].Variant)
		})
	}
}

func TestCanonicalSuppressionService(t *testing.T) {
	cases := []struct{ provider, service, want string }{
		{"aws", "ec2", "compute"},
		{"aws", "rds", "relational-db"},
		{"aws", "elasticache", "cache"},
		{"aws", "opensearch", "search"},
		{"aws", "redshift", "data-warehouse"},
		{"aws", "compute", "compute"},
		{"aws", "relational-db", "relational-db"},
		{"aws", "cache", "cache"},
		{"aws", "search", "search"},
		{"aws", "data-warehouse", "data-warehouse"},
		{"aws", "memorydb", "memorydb"},
		{"azure", "compute", "compute"},
		{"azure", "ec2", "ec2"},
		{"azure", "rds", "rds"},
		{"gcp", "compute", "compute"},
		{"gcp", "redshift", "redshift"},
		{"gcp", "elasticache", "elasticache"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.service, func(t *testing.T) {
			got := CanonicalSuppressionService(tc.provider, tc.service)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, CanonicalSuppressionService(tc.provider, got), "idempotent")
		})
	}
}

// Issue #1420: a legacy "rds" rec and a canonical "relational-db" rec for the
// same tuple merge into one canonical suppression row with the summed count.
func TestBuildSuppressions_MergesRDSAliases(t *testing.T) {
	gp := &GlobalConfig{GracePeriodDays: map[string]int{"aws": 7}}
	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "rds", Region: "r", ResourceType: "db.r5.large", Count: 3},
		{Provider: "aws", Service: "relational-db", Region: "r", ResourceType: "db.r5.large", Count: 2},
	}, "exec-1", gp, time.Now())
	require.Len(t, sups, 1)
	assert.Equal(t, "relational-db", sups[0].Service)
	assert.Equal(t, 5, sups[0].SuppressedCount)
}

// Issue #1412: AWS "ec2" and "compute" recs in one request share a bucket and
// are written under the canonical slug; Azure compute is untouched.
func TestBuildSuppressions_CanonicalisesAWSEC2Slug(t *testing.T) {
	now := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	gp := &GlobalConfig{GracePeriodDays: map[string]int{"aws": 7, "azure": 7}}
	sups := BuildSuppressions([]RecommendationRecord{
		{Provider: "aws", Service: "ec2", Region: "r", ResourceType: "m5.large", Count: 3},
		{Provider: "aws", Service: "compute", Region: "r", ResourceType: "m5.large", Count: 2},
		{Provider: "azure", Service: "compute", Region: "r", ResourceType: "D2", Count: 1},
	}, "exec-1", gp, now)
	require.Len(t, sups, 2)
	assert.Equal(t, "compute", sups[0].Service)
	assert.Equal(t, 5, sups[0].SuppressedCount)
	assert.Equal(t, "compute", sups[1].Service)
	assert.Equal(t, "azure", sups[1].Provider)
}
