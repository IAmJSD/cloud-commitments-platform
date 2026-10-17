package archera

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

const fixtureHourly = "0.50000000000000000001"

func storedMatchFixture() config.RecommendationRecord {
	source, cloud := "Source-account-a", "cloud-account-a"
	return config.RecommendationRecord{ID: "stored-recommendation-a", Provider: "aws",
		Service: "savings-plans-ec2instance", SourceAccountID: &source, CloudAccountID: &cloud,
		Term: 1, Payment: "no-upfront", Region: "us-east-1",
		Details: json.RawMessage(`{"offering_id":"provider-offer-a","plan_type":"EC2Instance","region":"us-east-1","instance_family":"m5","hourly_commitment":` + fixtureHourly + `}`)}
}

func withDetails(old, replacement string) config.RecommendationRecord {
	rec := storedMatchFixture()
	rec.Details = json.RawMessage(strings.Replace(string(rec.Details), old, replacement, 1))
	return rec
}

func TestProjectRecommendationMapsEveryField(t *testing.T) {
	rec := storedMatchFixture()
	got, err := ProjectRecommendation(&rec)
	require.NoError(t, err)
	want := RecommendationIdentity{Provider: "aws", SourceAccountID: "Source-account-a", CloudAccountID: "cloud-account-a",
		Service: "savings-plans-ec2instance", PlanType: "EC2Instance", OfferingID: "provider-offer-a",
		Region: "us-east-1", TopLevelRegion: "us-east-1", InstanceFamily: "m5", Payment: "no_upfront",
		NativeDurationSeconds: 31536000}
	hourly := got.HourlyCommitment
	require.Equal(t, "50000000000000000001/100000000000000000000", hourly.RatString())
	got.HourlyCommitment = nil
	require.Equal(t, want, *got)
}

func TestProjectRecommendationOptionalAccountsAndRegion(t *testing.T) {
	rec := storedMatchFixture()
	rec.CloudAccountID, rec.Region = nil, ""
	got, err := ProjectRecommendation(&rec)
	require.NoError(t, err)
	require.Empty(t, got.CloudAccountID)
	require.Empty(t, got.TopLevelRegion)
	rec.SourceAccountID = nil
	_, err = ProjectRecommendation(&rec)
	require.ErrorIs(t, err, ErrRecommendationIdentity)
}

func TestProjectRecommendationTermAndPayment(t *testing.T) {
	for term, seconds := range map[int]int64{1: 31536000, 3: 94608000} {
		rec := storedMatchFixture()
		rec.Term = term
		got, err := ProjectRecommendation(&rec)
		require.NoError(t, err)
		require.Equal(t, seconds, got.NativeDurationSeconds)
	}
	for stored, wire := range map[string]string{"no-upfront": "no_upfront", "partial-upfront": "partial_upfront", "all-upfront": "all_upfront"} {
		rec := storedMatchFixture()
		rec.Payment = stored
		got, err := ProjectRecommendation(&rec)
		require.NoError(t, err, stored)
		require.Equal(t, wire, got.Payment)
	}
	for _, bad := range []string{"", "NO-UPFRONT", "partial", "monthly", "no_upfront"} {
		rec := storedMatchFixture()
		rec.Payment = bad
		_, err := ProjectRecommendation(&rec)
		require.ErrorIs(t, err, ErrRecommendationIdentity, bad)
	}
	for _, term := range []int{0, 2, 5, -1} {
		rec := storedMatchFixture()
		rec.Term = term
		_, err := ProjectRecommendation(&rec)
		require.ErrorIs(t, err, ErrRecommendationIdentity, term)
	}
}

func TestProjectRecommendationHourlyAmount(t *testing.T) {
	refused := map[string]string{
		"nil": "null", "zero": "0", "negative": "-0.5", "string": `"abc"`, "quoted": `"0.5"`,
		"boolean": "true", "object": "{}", "array": "[0.5]", "bytes_65": "0." + strings.Repeat("1", 63),
		"huge_exponent": "1e999999", "huge_negative_exponent": "1e-999999", "exp_101": "1e101",
		"exp_minus_101": "1e-101", "overflow_exponent": "1e999999999999999999999",
		"leading_plus": "+1", "fraction": "1/2", "empty_exponent": "1e", "zero_exp": "0e5",
	}
	for name, value := range refused {
		t.Run(name, func(t *testing.T) {
			rec := withDetails(fixtureHourly, value)
			got, err := ProjectRecommendation(&rec)
			require.ErrorIs(t, err, ErrRecommendationIdentity)
			require.Nil(t, got)
		})
	}
	missing := withDetails(`,"hourly_commitment":`+fixtureHourly, "")
	_, err := ProjectRecommendation(&missing)
	require.ErrorIs(t, err, ErrRecommendationIdentity)
	for _, value := range []string{"1e100", "1e-100", "1E2", "1e-07", "0." + strings.Repeat("1", 62), "0.25"} {
		rec := withDetails(fixtureHourly, value)
		got, err := ProjectRecommendation(&rec)
		require.NoError(t, err, value)
		want, ok := new(big.Rat).SetString(value)
		require.True(t, ok)
		require.Zero(t, got.HourlyCommitment.Cmp(want), value)
	}
}

func TestProjectRecommendationRefusesBadDetails(t *testing.T) {
	tests := []struct{ name, old, replacement string }{
		{"missing_offering", `"offering_id":"provider-offer-a",`, ""},
		{"null_offering", `"offering_id":"provider-offer-a"`, `"offering_id":null`},
		{"missing_plan", `"plan_type":"EC2Instance",`, ""},
		{"wrong_plan", `"plan_type":"EC2Instance"`, `"plan_type":"Compute"`},
		{"empty_region", `"region":"us-east-1"`, `"region":""`},
		{"wrong_region_kind", `"region":"us-east-1"`, `"region":["us-east-1"]`},
		{"missing_family", `"instance_family":"m5",`, ""},
		{"wrong_family_kind", `"instance_family":"m5"`, `"instance_family":2`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := withDetails(tt.old, tt.replacement)
			got, err := ProjectRecommendation(&rec)
			require.ErrorIs(t, err, ErrRecommendationIdentity)
			require.Nil(t, got)
		})
	}
	for _, raw := range []string{"", "null", "[]", "{}", string(storedMatchFixture().Details) + "{}", strings.Repeat(" ", 65537)} {
		rec := storedMatchFixture()
		rec.Details = json.RawMessage(raw)
		_, err := ProjectRecommendation(&rec)
		require.ErrorIs(t, err, ErrRecommendationIdentity)
	}
	padded := storedMatchFixture()
	padded.Details = json.RawMessage(string(padded.Details) + strings.Repeat(" ", 65537))
	_, err := ProjectRecommendation(&padded)
	require.ErrorIs(t, err, ErrRecommendationIdentity)
	_, err = ProjectRecommendation(nil)
	require.ErrorIs(t, err, ErrRecommendationIdentity)
}

func TestProjectRecommendationRequiresSupportedProduct(t *testing.T) {
	for field, mutate := range map[string]func(*config.RecommendationRecord){
		"provider":   func(r *config.RecommendationRecord) { r.Provider = "gcp" },
		"service":    func(r *config.RecommendationRecord) { r.Service = "savings-plans-compute" },
		"top_region": func(r *config.RecommendationRecord) { r.Region = "us-west-2" },
		"source":     func(r *config.RecommendationRecord) { r.SourceAccountID = nil },
	} {
		rec := storedMatchFixture()
		mutate(&rec)
		_, err := ProjectRecommendation(&rec)
		require.ErrorIs(t, err, ErrRecommendationIdentity, field)
	}
}

func TestRecommendationIdentityEqual(t *testing.T) {
	project := func(mutate func(*config.RecommendationRecord)) *RecommendationIdentity {
		rec := storedMatchFixture()
		mutate(&rec)
		got, err := ProjectRecommendation(&rec)
		require.NoError(t, err)
		return got
	}
	base := project(func(*config.RecommendationRecord) {})
	same := project(func(r *config.RecommendationRecord) {
		r.Details = json.RawMessage(strings.Replace(string(r.Details), fixtureHourly, "0.500000000000000000010", 1))
	})
	require.True(t, base.Equal(same), "numerically equal hourly amounts are equal")
	require.True(t, base.Equal(base))
	other := "other"
	changed := map[string]*RecommendationIdentity{
		"hourly": project(func(r *config.RecommendationRecord) {
			r.Details = json.RawMessage(strings.Replace(string(r.Details), fixtureHourly, "0.50000000000000000002", 1))
		}),
		"offering_id": project(func(r *config.RecommendationRecord) {
			r.Details = json.RawMessage(strings.Replace(string(r.Details), "provider-offer-a", "provider-offer-b", 1))
		}),
		"plan_type":     {},
		"cloud_account": project(func(r *config.RecommendationRecord) { r.CloudAccountID = &other }),
		"cloud_nil":     project(func(r *config.RecommendationRecord) { r.CloudAccountID = nil }),
		"source":        project(func(r *config.RecommendationRecord) { r.SourceAccountID = &other }),
		"term":          project(func(r *config.RecommendationRecord) { r.Term = 3 }),
		"payment":       project(func(r *config.RecommendationRecord) { r.Payment = "all-upfront" }),
		"family": project(func(r *config.RecommendationRecord) {
			r.Details = json.RawMessage(strings.Replace(string(r.Details), `"m5"`, `"c5"`, 1))
		}),
		"region": project(func(r *config.RecommendationRecord) {
			r.Region = ""
			r.Details = json.RawMessage(strings.Replace(string(r.Details), `"region":"us-east-1"`, `"region":"us-west-2"`, 1))
		}),
		"top_region": project(func(r *config.RecommendationRecord) { r.Region = "" }),
	}
	for name, got := range changed {
		require.False(t, base.Equal(got), name)
		require.False(t, got.Equal(base), name)
	}
	noHourly := *base
	noHourly.HourlyCommitment = nil
	for name, pair := range map[string][2]*RecommendationIdentity{
		"nil_receiver": {nil, base}, "nil_arg": {base, nil}, "both_nil": {nil, nil},
		"nil_hourly_recv": {&noHourly, base}, "nil_hourly_arg": {base, &noHourly},
	} {
		require.False(t, pair[0].Equal(pair[1]), name)
	}
	require.True(t, noHourly.Equal(&noHourly))
}
