package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real regional EC2 Details shape as persisted by the scheduler (issue #806).
func ec2Details(platform, tenancy string) json.RawMessage {
	return json.RawMessage(`{"instance_type":"m5.large","platform":"` + platform + `","tenancy":"` + tenancy + `","scope":"Region"}`)
}

func ec2PlatformRec(platform, tenancy string, upfront float64) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: "rec-" + platform + "-" + tenancy, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Term: 1, Payment: "all-upfront", Count: 1, UpfrontCost: upfront, Savings: 10,
		Details: ec2Details(platform, tenancy),
	}
}

func TestRecIdentityKey_DiffersByPlatformAndTenancy(t *testing.T) {
	linux := ec2PlatformRec("Linux/UNIX", "default", 1)
	windows := ec2PlatformRec("Windows", "default", 1)
	dedicated := ec2PlatformRec("Linux/UNIX", "dedicated", 1)
	assert.NotEqual(t, mustRecIdentityKey(t, &linux), mustRecIdentityKey(t, &windows))
	assert.NotEqual(t, mustRecIdentityKey(t, &linux), mustRecIdentityKey(t, &dedicated))
	same := ec2PlatformRec("Linux/UNIX", "default", 99)
	assert.Equal(t, mustRecIdentityKey(t, &linux), mustRecIdentityKey(t, &same))
}

func TestRecIdentityKey_MalformedDetailsErrors(t *testing.T) {
	bad := ec2PlatformRec("Linux/UNIX", "default", 1)
	bad.Details = json.RawMessage(`{"platform":5}`)
	_, err := config.RecIdentityKey(&bad)
	require.Error(t, err)
}

// Rows differing only by platform (or only by tenancy) must both load (no
// "more than one row" refusal) and the request, which carries the values only
// in Details, is priced from its own variant's row.
func TestPriceRecommendationsFromStore_PricesFromRequestedPlatformAndTenancy(t *testing.T) {
	for name, want := range map[string]struct {
		platform, tenancy string
		upfront           float64
	}{
		"windows":   {"Windows", "default", 900},
		"dedicated": {"Linux/UNIX", "dedicated", 500},
		"linux":     {"Linux/UNIX", "default", 100},
	} {
		t.Run(name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			expectStoredRecs(store,
				ec2PlatformRec("Linux/UNIX", "default", 100),
				ec2PlatformRec("Windows", "default", 900),
				ec2PlatformRec("Linux/UNIX", "dedicated", 500))
			h := &Handler{config: store}
			req := []config.RecommendationRecord{{
				Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
				Term: 1, Payment: "all-upfront", Count: 1, Details: ec2Details(want.platform, want.tenancy),
			}}
			require.NoError(t, h.priceRecommendationsFromStore(context.Background(), req))
			assert.Equal(t, want.upfront, req[0].UpfrontCost)
		})
	}
}

// An external client that sends no Details cannot name its platform: it is
// refused with a 409 rather than priced off an arbitrary variant.
func TestPriceRecommendationsFromStore_RequestWithoutDetailsRefusedForEC2(t *testing.T) {
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	expectStoredRecs(store, ec2PlatformRec("Linux/UNIX", "default", 100))
	h := &Handler{config: store}
	req := []config.RecommendationRecord{{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Term: 1, Payment: "all-upfront", Count: 1,
	}}
	err := h.priceRecommendationsFromStore(context.Background(), req)
	require.Error(t, err)
	ce, isClient := IsClientError(err)
	require.True(t, isClient)
	assert.Equal(t, 409, ce.code)
}

func TestPurchaseIdempotencyKey_DiffersByPlatformAndTenancy(t *testing.T) {
	t.Parallel()
	key := func(platform, tenancy string) string {
		return mustPurchaseIdempotencyKey("u", []config.RecommendationRecord{ec2PlatformRec(platform, tenancy, 1)}, 100)
	}
	assert.NotEqual(t, key("Linux/UNIX", "default"), key("Windows", "default"),
		"a Windows submit right after a Linux one must not dedupe into it")
	assert.NotEqual(t, key("Linux/UNIX", "default"), key("Linux/UNIX", "dedicated"))
	assert.Equal(t, key("Linux/UNIX", "default"), key("Linux/UNIX", "default"))
}

func TestDescribeRec_IncludesPlatformAndTenancy(t *testing.T) {
	r := ec2PlatformRec("Windows", "dedicated", 1)
	assert.Contains(t, describeRec(&r), `platform="Windows" tenancy="dedicated"`)
	bare := config.RecommendationRecord{Provider: "aws", Service: "rds"}
	assert.NotContains(t, describeRec(&bare), "platform=")
}

// The detail route recovers an ID containing "/" and spaces (AWS platform
// strings such as "Linux/UNIX" and "Red Hat Enterprise Linux", issue #806).
// The frontend sends encodeURIComponent(id); net/http hands the router the
// decoded path.
func TestRouter_RecommendationDetailRouteKeepsPlatformInID(t *testing.T) {
	id := "aws|acct|ec2|us-east-1|m5.large||1|all-upfront|Red Hat Enterprise Linux|default"
	idSlash := "aws|acct|ec2|us-east-1|m5.large||1|all-upfront|Linux/UNIX|default"
	r := NewRouter(&Handler{})
	for _, want := range []string{id, idSlash} {
		path := "/api/recommendations/" + want + "/detail"
		var found *Route
		for i := range r.routes {
			if r.matches(r.routes[i], "GET", path) {
				found = &r.routes[i]
				break
			}
		}
		require.NotNil(t, found, path)
		assert.Equal(t, "/detail", found.PathSuffix)
		assert.Equal(t, want, r.extractParams(*found, path)["id"])
	}
}
