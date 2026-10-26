package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSuppressionStore is a minimal in-memory stand-in for the
// scheduler's StoreInterface. We only implement the two methods the
// applySuppressions path touches plus the default no-ops forwarded
// through the embedded MockConfigStore in scheduler_test.go.
type mockSuppressionStore struct {
	MockConfigStore
	recs []config.RecommendationRecord
	sups []config.PurchaseSuppression
}

func (m *mockSuppressionStore) ListStoredRecommendations(_ context.Context, _ config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return m.recs, nil
}
func (m *mockSuppressionStore) ListActiveSuppressions(_ context.Context) ([]config.PurchaseSuppression, error) {
	return m.sups, nil
}
func (m *mockSuppressionStore) GetRecommendationsFreshness(_ context.Context) (*config.RecommendationsFreshness, error) {
	now := time.Now()
	return &config.RecommendationsFreshness{LastCollectedAt: &now}, nil
}
func (m *mockSuppressionStore) MarkCollectionStarted(_ context.Context) (string, bool, error) {
	return "mock-owner-token", true, nil
}
func (m *mockSuppressionStore) ClearCollectionStarted(_ context.Context, _ string) error {
	return nil
}

// GetServiceConfig / GetAccountServiceOverride: required by the override
// helper that runs inside ListRecommendations after the suppression pass
// (issue #196). Returning nil signals "no global config registered" — the
// resolver then leaves the rec un-filtered, which is what the suppression
// tests want (they assert on the suppression counter, not on overrides).
func (m *mockSuppressionStore) GetServiceConfig(_ context.Context, _, _ string) (*config.ServiceConfig, error) {
	return nil, nil
}
func (m *mockSuppressionStore) GetAccountServiceOverride(_ context.Context, _, _, _ string) (*config.AccountServiceOverride, error) {
	return nil, nil
}

// GetGlobalConfig returns a default config so ListRecommendations can resolve
// the effective stale TTL without panicking on the embedded MockConfigStore.
func (m *mockSuppressionStore) GetGlobalConfig(_ context.Context) (*config.GlobalConfig, error) {
	return &config.GlobalConfig{
		RecommendationsCacheStaleHours: config.DefaultRecommendationsCacheStaleHours,
		RecommendationsLookbackDays:    config.DefaultRecommendationsLookbackDays,
	}, nil
}
func (m *mockSuppressionStore) UpsertNotificationMute(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mockSuppressionStore) IsNotificationMuted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func strPtr(s string) *string { return &s }

func TestApplySuppressions_SubtractsCount(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	store := &mockSuppressionStore{
		recs: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1",
				ResourceType: "t4g.nano", Count: 5,
				CloudAccountID: strPtr("acct-1")},
		},
		sups: []config.PurchaseSuppression{
			{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
				Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano",
				SuppressedCount: 3, ExpiresAt: future},
		},
	}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 2, recs[0].Count, "5 - 3 = 2")
	assert.Equal(t, 3, recs[0].SuppressedCount)
	require.NotNil(t, recs[0].SuppressionExpiresAt)
	assert.WithinDuration(t, future, *recs[0].SuppressionExpiresAt, time.Second)
	require.NotNil(t, recs[0].PrimarySuppressionExecutionID)
	assert.Equal(t, "e1", *recs[0].PrimarySuppressionExecutionID)
}

func TestApplySuppressions_DropsFullyCoveredRecs(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	store := &mockSuppressionStore{
		recs: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1",
				ResourceType: "t4g.nano", Count: 5,
				CloudAccountID: strPtr("acct-1")},
		},
		sups: []config.PurchaseSuppression{
			{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
				Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano",
				SuppressedCount: 5, ExpiresAt: future},
		},
	}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	assert.Empty(t, recs, "rec fully covered by suppression should be dropped")
}

func TestApplySuppressions_CumulativeAcrossExecutions(t *testing.T) {
	ctx := context.Background()
	early := time.Now().Add(12 * time.Hour)
	later := time.Now().Add(48 * time.Hour)
	store := &mockSuppressionStore{
		recs: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1",
				ResourceType: "t4g.nano", Count: 10,
				CloudAccountID: strPtr("acct-1")},
		},
		sups: []config.PurchaseSuppression{
			{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
				Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano",
				SuppressedCount: 3, ExpiresAt: later, CreatedAt: time.Now().Add(-time.Hour)},
			{ExecutionID: "e2", AccountID: "acct-1", Provider: "aws",
				Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano",
				SuppressedCount: 4, ExpiresAt: early, CreatedAt: time.Now()},
		},
	}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 3, recs[0].Count, "10 - 3 - 4 = 3")
	assert.Equal(t, 7, recs[0].SuppressedCount)
	require.NotNil(t, recs[0].SuppressionExpiresAt)
	assert.WithinDuration(t, early, *recs[0].SuppressionExpiresAt, time.Second,
		"earliest expiry across contributors wins")
	// Primary = most-contributing execution (4 > 3 → e2)
	require.NotNil(t, recs[0].PrimarySuppressionExecutionID)
	assert.Equal(t, "e2", *recs[0].PrimarySuppressionExecutionID)
}

func TestApplySuppressions_EngineDifferentiates(t *testing.T) {
	// A suppression for engine='' must NOT match a rec with
	// engine='postgres' (and vice versa). Pins the 6-tuple match rule.
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	store := &mockSuppressionStore{
		recs: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "rds", Region: "us-east-1",
				ResourceType: "db.t4g.micro", Engine: "postgres", Count: 5,
				CloudAccountID: strPtr("acct-1")},
		},
		sups: []config.PurchaseSuppression{
			// Suppression with empty engine — different 6-tuple.
			{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
				Service: "rds", Region: "us-east-1", ResourceType: "db.t4g.micro",
				Engine: "", SuppressedCount: 5, ExpiresAt: future},
		},
	}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 5, recs[0].Count, "engine mismatch → no subtraction")
	assert.Equal(t, 0, recs[0].SuppressedCount)
}

func TestApplySuppressions_NilAccountIDNormalised(t *testing.T) {
	// A rec with CloudAccountID=nil should match a suppression row
	// with account_id='' (and vice versa). Pins the normalisation
	// rule in applySuppressions.
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	store := &mockSuppressionStore{
		recs: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1",
				ResourceType: "t4g.nano", Count: 5, CloudAccountID: nil},
		},
		sups: []config.PurchaseSuppression{
			{ExecutionID: "e1", AccountID: "", Provider: "aws",
				Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano",
				SuppressedCount: 2, ExpiresAt: future},
		},
	}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 3, recs[0].Count, "5 - 2 = 3 (nil account matched to empty-string suppression)")
}

// TestApplySuppressionIndex_DoesNotMutateCallerSlice asserts that
// applySuppressionIndex allocates a fresh output slice and never writes back
// into the caller's backing array (05-M1).
//
// Pre-fix: out := recs[:0] shared the caller's backing array, so any append
// inside the function would overwrite elements that the caller still expected
// at their original positions. The fix allocates make([]T, 0, len(recs)).
func TestApplySuppressionIndex_DoesNotMutateCallerSlice(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	// Two recs; only the second is suppressed and therefore dropped.
	recs := []config.RecommendationRecord{
		{ID: "keep", Provider: "aws", Service: "ec2", Region: "us-east-1",
			ResourceType: "t4g.nano", Count: 5, CloudAccountID: strPtr("acct-1")},
		{ID: "drop", Provider: "aws", Service: "ec2", Region: "us-east-2",
			ResourceType: "t4g.nano", Count: 5, CloudAccountID: strPtr("acct-1")},
	}
	// Keep a copy of the original IDs so we can detect mutations.
	originalIDs := []string{recs[0].ID, recs[1].ID}

	sups := []config.PurchaseSuppression{
		{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
			Service: "ec2", Region: "us-east-2", ResourceType: "t4g.nano",
			SuppressedCount: 5, ExpiresAt: future},
	}
	index := aggregateSuppressions(sups)
	out := applySuppressionIndex(recs, index)

	// The survivor set must contain only the non-suppressed rec.
	require.Len(t, out, 1)
	assert.Equal(t, "keep", out[0].ID)

	// The original slice must be unchanged: both elements at their
	// original positions with the original IDs (pre-fix: recs[1].ID
	// would be overwritten by "keep" since out alias the same array).
	require.Len(t, recs, 2, "original slice length must not change")
	assert.Equal(t, originalIDs[0], recs[0].ID, "recs[0] must not be mutated")
	assert.Equal(t, originalIDs[1], recs[1].ID, "recs[1] must not be mutated")
}

// Suppress unused-import warning in case this is the only file that imports pgx.
var _ pgx.Tx = (pgx.Tx)(nil)

// ec2VariantRec is a zonal or regional EC2 rec whose platform and tenancy live
// in Details, as the collector stores them.
func ec2VariantRec(id, az, platform, tenancy string) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: id, Provider: "aws", Service: "ec2", Region: "us-east-1",
		ResourceType: "m5.large", Count: 5, CloudAccountID: strPtr("acct-1"),
		AvailabilityZone: az,
		Details:          []byte(`{"availability_zone":"` + az + `","platform":"` + platform + `","tenancy":"` + tenancy + `"}`),
	}
}

// TestApplySuppressions_OnlyThePurchasedVariant is the issue #816 regression:
// buying the us-east-1a Linux/default variant must reduce only that rec. Before
// the fix the 6-tuple key subtracted the purchase from the us-east-1b,
// regional, Windows and dedicated siblings too, hiding them for the whole
// grace window. The suppressions come from the real BuildSuppressions, so the
// write and read sides are exercised together.
func TestApplySuppressions_OnlyThePurchasedVariant(t *testing.T) {
	ctx := context.Background()
	bought := ec2VariantRec("bought", "us-east-1a", "Linux/UNIX", "default")
	siblings := []config.RecommendationRecord{
		ec2VariantRec("other-az", "us-east-1b", "Linux/UNIX", "default"),
		ec2VariantRec("regional", "", "Linux/UNIX", "default"),
		ec2VariantRec("windows", "us-east-1a", "Windows", "default"),
		ec2VariantRec("dedicated", "us-east-1a", "Linux/UNIX", "dedicated"),
	}
	purchase := bought
	purchase.Count = 3
	sups := config.BuildSuppressions([]config.RecommendationRecord{purchase}, "e1",
		&config.GlobalConfig{GracePeriodDays: map[string]int{"aws": 7}}, time.Now())
	require.Len(t, sups, 1)

	store := &mockSuppressionStore{recs: append([]config.RecommendationRecord{bought}, siblings...), sups: sups}
	s := &Scheduler{config: store}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	got := map[string]config.RecommendationRecord{}
	for _, r := range recs {
		got[r.ID] = r
	}
	require.Len(t, got, 5, "no sibling may be hidden by the purchase")
	assert.Equal(t, 2, got["bought"].Count, "5 - 3 on the purchased variant")
	assert.Equal(t, 3, got["bought"].SuppressedCount)
	for _, sib := range siblings {
		assert.Equal(t, 5, got[sib.ID].Count, "%s must keep its full count", sib.ID)
		assert.Zero(t, got[sib.ID].SuppressedCount, "%s must carry no suppression badge", sib.ID)
		assert.Nil(t, got[sib.ID].PrimarySuppressionExecutionID, sib.ID)
	}
}

// TestApplySuppressions_LegacyRowIsAWildcard pins the expand/contract rule of
// issue #816: a row without a variant (written before migration 000134, or by
// pre-#816 code during a rolling deploy) still applies to every rec on its
// 6-tuple, as before, while a variant row applies only to its own variant and
// adds to the legacy count there.
func TestApplySuppressions_LegacyRowIsAWildcard(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	legacy := config.PurchaseSuppression{ExecutionID: "legacy", AccountID: "acct-1", Provider: "aws",
		Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", SuppressedCount: 1, ExpiresAt: future}
	scoped := legacy
	scoped.ExecutionID = "scoped"
	scoped.SuppressedCount = 2
	scoped.Variant = &config.SuppressionVariant{AvailabilityZone: "us-east-1a", Platform: "Linux/UNIX", Tenancy: "default"}

	recs := []config.RecommendationRecord{
		ec2VariantRec("a-linux", "us-east-1a", "Linux/UNIX", "default"),
		ec2VariantRec("b-linux", "us-east-1b", "Linux/UNIX", "default"),
	}
	out := applySuppressionIndex(recs, aggregateSuppressions([]config.PurchaseSuppression{legacy, scoped}))
	require.Len(t, out, 2)
	assert.Equal(t, 2, out[0].Count, "a-linux: 5 - 1 legacy - 2 scoped")
	assert.Equal(t, 3, out[0].SuppressedCount)
	require.NotNil(t, out[0].PrimarySuppressionExecutionID)
	assert.Equal(t, "scoped", *out[0].PrimarySuppressionExecutionID)
	assert.Equal(t, 4, out[1].Count, "b-linux: only the legacy wildcard applies")
	require.NotNil(t, out[1].PrimarySuppressionExecutionID)
	assert.Equal(t, "legacy", *out[1].PrimarySuppressionExecutionID)
}

// TestApplySuppressions_NonComputeVariantMatches: a non-EC2 rec has an empty
// variant (empty AZ, platform and tenancy), which is a known value and must match
// the row its own purchase wrote, exactly as the 6-tuple did before #816.
func TestApplySuppressions_NonComputeVariantMatches(t *testing.T) {
	rds := config.RecommendationRecord{ID: "rds", Provider: "aws", Service: "rds", Region: "us-east-1",
		ResourceType: "db.t4g.micro", Engine: "postgres", Count: 5, CloudAccountID: strPtr("acct-1"),
		Details: []byte(`{"engine":"postgres","deployment_option":"single-az"}`)}
	purchase := rds
	purchase.Count = 2
	sups := config.BuildSuppressions([]config.RecommendationRecord{purchase}, "e1", &config.GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	require.NotNil(t, sups[0].Variant)

	out := applySuppressionIndex([]config.RecommendationRecord{rds}, aggregateSuppressions(sups))
	require.Len(t, out, 1)
	assert.Equal(t, 3, out[0].Count)
}

// TestApplySuppressions_UnreadableRecDetailsMatchEveryRow: a rec whose Details
// cannot be read has no known variant, so every row on its 6-tuple applies
// (the pre-#816 behavior: over-suppress, never over-show).
func TestApplySuppressions_UnreadableRecDetailsMatchEveryRow(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	sup := config.PurchaseSuppression{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws",
		Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", SuppressedCount: 2, ExpiresAt: future,
		Variant: &config.SuppressionVariant{AvailabilityZone: "us-east-1a", Platform: "Linux/UNIX", Tenancy: "default"}}
	rec := ec2VariantRec("broken", "us-east-1b", "Windows", "default")
	rec.Details = []byte(`{"platform":42}`)

	out := applySuppressionIndex([]config.RecommendationRecord{rec}, aggregateSuppressions([]config.PurchaseSuppression{sup}))
	require.Len(t, out, 1)
	assert.Equal(t, 3, out[0].Count)
}

// TestApplySuppressions_AWSLegacySlugSpellings covers issues #1412, #1420 and #1422: AWS
// recs and suppressions can be stored under a legacy or canonical slug, and a
// suppression hides the rec whichever spelling each uses. Azure and GCP
// "compute" stay distinct from "ec2".
func TestApplySuppressions_AWSLegacySlugSpellings(t *testing.T) {
	cases := []struct {
		name, provider, recService, supService string
		wantCount                              int
	}{
		{"aws compute sup hides ec2 rec", "aws", "ec2", "compute", 2},
		{"aws ec2 sup hides compute rec", "aws", "compute", "ec2", 2},
		{"aws same slug compute", "aws", "compute", "compute", 2},
		{"aws unrelated service not hidden", "aws", "ec2", "rds", 5},
		{"aws legacy rds sup hides relational-db rec", "aws", "relational-db", "rds", 2},
		{"aws relational-db sup hides legacy rds rec", "aws", "rds", "relational-db", 2},
		{"aws legacy elasticache sup hides cache rec", "aws", "cache", "elasticache", 2},
		{"aws cache sup hides legacy elasticache rec", "aws", "elasticache", "cache", 2},
		{"aws legacy opensearch sup hides search rec", "aws", "search", "opensearch", 2},
		{"aws search sup hides legacy opensearch rec", "aws", "opensearch", "search", 2},
		{"aws legacy redshift sup hides data-warehouse rec", "aws", "data-warehouse", "redshift", 2},
		{"aws data-warehouse sup hides legacy redshift rec", "aws", "redshift", "data-warehouse", 2},
		{"aws memorydb sup does not hide cache rec", "aws", "cache", "memorydb", 5},
		{"aws dash-free compute SP sup hides compute SP rec", "aws", "savings-plans-compute", "savingsplans-compute", 2},
		{"aws compute SP sup hides dash-free compute SP rec", "aws", "savingsplans-compute", "savings-plans-compute", 2},
		{"aws dash-free database SP sup hides database SP rec", "aws", "savings-plans-database", "savingsplans-database", 2},
		{"aws umbrella SP sup does not hide compute SP rec", "aws", "savings-plans-compute", "savingsplans", 5},
		{"aws compute SP sup does not hide sagemaker SP rec", "aws", "savings-plans-sagemaker", "savingsplans-compute", 5},
		{"azure legacy rds sup does not hide relational-db rec", "azure", "relational-db", "rds", 5},
		{"azure compute sup hides compute rec", "azure", "compute", "compute", 2},
		{"azure ec2 sup does not hide compute rec", "azure", "compute", "ec2", 5},
		{"gcp compute sup does not hide ec2 rec", "gcp", "ec2", "compute", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockSuppressionStore{
				recs: []config.RecommendationRecord{{ID: "r1", Provider: tc.provider, Service: tc.recService,
					Region: "r", ResourceType: "m5.large", Count: 5}},
				sups: []config.PurchaseSuppression{{ExecutionID: "e1", Provider: tc.provider, Service: tc.supService,
					Region: "r", ResourceType: "m5.large", SuppressedCount: 3, ExpiresAt: time.Now().Add(time.Hour)}},
			}
			s := &Scheduler{config: store}
			recs, err := s.ListRecommendations(context.Background(), config.RecommendationFilter{})
			require.NoError(t, err)
			require.Len(t, recs, 1)
			assert.Equal(t, tc.wantCount, recs[0].Count)
		})
	}
}

// TestApplySuppressions_LegacyEC2Shapes covers the two legacy shapes of issue
// #1232 end to end: a suppression stored with tenancy "shared", or built from a
// rec whose AZ lives only in Details, must still hide the modern rec.
func TestApplySuppressions_LegacyEC2Shapes(t *testing.T) {
	modern := ec2VariantRec("modern", "us-east-1a", "Linux/UNIX", "default")
	azOnlyInDetails := modern
	azOnlyInDetails.AvailabilityZone = ""
	sharedPurchase := ec2VariantRec("p", "us-east-1a", "Linux/UNIX", "shared")
	exp := time.Now().Add(time.Hour)
	for name, tc := range map[string]struct {
		rec  config.RecommendationRecord
		sups []config.PurchaseSuppression
		want int
	}{
		"stored shared row vs default rec": {modern, []config.PurchaseSuppression{{ExecutionID: "e", AccountID: "acct-1", Provider: "aws", Service: "compute", Region: "us-east-1",
			ResourceType: "m5.large", SuppressedCount: 2, ExpiresAt: exp, Variant: &config.SuppressionVariant{AvailabilityZone: "us-east-1a", Platform: "Linux/UNIX", Tenancy: "shared"}}}, 3},
		"built from shared rec": {modern, config.BuildSuppressions([]config.RecommendationRecord{withCount(sharedPurchase, 2)}, "e",
			&config.GlobalConfig{GracePeriodDays: map[string]int{"aws": 7}}, time.Now()), 3},
		"rec AZ only in details, row has AZ": {azOnlyInDetails, config.BuildSuppressions([]config.RecommendationRecord{withCount(modern, 2)}, "e",
			&config.GlobalConfig{GracePeriodDays: map[string]int{"aws": 7}}, time.Now()), 3},
		"purchase AZ only in details": {modern, config.BuildSuppressions([]config.RecommendationRecord{withCount(azOnlyInDetails, 2)}, "e",
			&config.GlobalConfig{GracePeriodDays: map[string]int{"aws": 7}}, time.Now()), 3},
	} {
		t.Run(name, func(t *testing.T) {
			index := aggregateSuppressions(tc.sups)
			out := applySuppressionIndex([]config.RecommendationRecord{tc.rec}, index)
			require.Len(t, out, 1)
			assert.Equal(t, tc.want, out[0].Count)
		})
	}
}

func withCount(r config.RecommendationRecord, n int) config.RecommendationRecord {
	r.Count = n
	return r
}
