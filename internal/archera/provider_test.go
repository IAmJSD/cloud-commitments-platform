package archera

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/secrets"
)

// All values are synthetic.
const (
	testKey    = "synthetic-archera-key-0123456789"
	testOrg    = "11111111-1111-4111-8111-111111111111"
	testPlan   = "44444444-4444-4444-8444-444444444444"
	testLineID = "22222222-2222-4222-8222-222222222222"
	testSecret = "SYNTHETIC_ARCHERA_KEY_REF"
	financials = `{"commitment_cost":{"total":110,"breakdown":{"cloud_provider_cost":{"total":100},"archera_premium":10}},"commitment_savings":{"net":-5.5,"gross":4.5},"covered_ondemand_cost":104.5}`
	offerEntry = `{"is_current":true,"offer_id":"11111111-1111-4111-8111-111111111111","offer_org_id":"public","offer":{"provider":"aws","type":"aws/AmazonEC2","region":"us-east-1","guaranteed_display_name":null},"lease_menu_item_id":null,"selected_amount":3,"commitment_type":"aws/AmazonEC2","contract_term":null,"payment_option":null,"discount_rate":0.3,"breakeven_days":null,"commitment_upfront_cost":1200,"commitment_financials_monthly_rate":` + financials + `,"delta_vs_current":{"monthly_net_savings":0,"upfront_cost":0,"discount_rate":0,"breakeven_days":null}}`
	comparison = `{"current_totals":{"commitment_financials_monthly_rate":` + financials + `,"commitment_upfront_cost":1200},"hypothetical_totals":[],"data":[{"line_item_id":"` + testLineID + `","current":` + offerEntry + `,"candidates":[]}]}`
)

// hostAssertingTransport fails the test unless the request targets the fixed
// Archera origin, then serves a synthetic response in place of the network.
type hostAssertingTransport struct {
	t               *testing.T
	calls           atomic.Int32
	body            string
	code            int
	gotKey, gotPath string
}

func (rt *hostAssertingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	assert.Equal(rt.t, "api.archera.ai", r.URL.Host)
	assert.Equal(rt.t, "https", r.URL.Scheme)
	rt.gotKey, rt.gotPath = r.Header.Get("x-api-key"), r.URL.Path
	code := rt.code
	if code == 0 {
		code = 200
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(rt.body)), Request: r}, nil
}

func fullSettings() Settings {
	return Settings{OrgID: testOrg, PlanID: testPlan, KeySecretRef: testSecret}
}

func envResolver(t *testing.T) secrets.Resolver {
	t.Helper()
	t.Setenv(testSecret, testKey)
	return secrets.NewEnvResolver()
}

func TestStatus_NamesOnly(t *testing.T) {
	cases := []struct {
		name    string
		s       Settings
		missing []string
		ok      bool
	}{
		{"off", Settings{}, []string{EnvKeySecret, EnvOrgID, EnvPlanID}, false},
		{"no key", Settings{OrgID: testOrg, PlanID: testPlan}, []string{EnvKeySecret}, false},
		{"no org", Settings{PlanID: testPlan, KeySecretRef: testSecret}, []string{EnvOrgID}, false},
		{"no plan", Settings{OrgID: testOrg, KeySecretRef: testSecret}, []string{EnvPlanID}, false},
		{"complete", fullSettings(), []string{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &hostAssertingTransport{t: t}
			st := NewProvider(tc.s, nil, &http.Client{Transport: rt}).Status()
			assert.Equal(t, tc.ok, st.Configured)
			assert.Equal(t, tc.missing, st.Missing)
			assert.Zero(t, rt.calls.Load(), "status must make no outbound call")
			assert.NotContains(t, fmt.Sprintf("%+v", st), testOrg)
		})
	}
}

func TestStatus_NilProviderIsOff(t *testing.T) {
	var p *Provider
	assert.False(t, p.Status().Configured)
	_, err := p.Client(context.Background())
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestClient_IncompleteConfigNamesMissingSettingAndSendsNothing(t *testing.T) {
	rt := &hostAssertingTransport{t: t}
	p := NewProvider(Settings{OrgID: testOrg, KeySecretRef: testSecret}, envResolver(t), &http.Client{Transport: rt})
	_, err := p.Client(context.Background())
	require.ErrorIs(t, err, ErrNotConfigured)
	assert.Contains(t, err.Error(), EnvPlanID)
	assert.NotContains(t, err.Error(), testKey)
	assert.Zero(t, rt.calls.Load())
}

type flakyResolver struct {
	calls atomic.Int32
	fails int32
}

func (f *flakyResolver) GetSecret(context.Context, string) (string, error) {
	if f.calls.Add(1) <= f.fails {
		// The error text carries the key to prove it is not propagated.
		return "", errors.New("vault down while reading " + testKey)
	}
	return testKey, nil
}
func (*flakyResolver) GetSecretJSON(context.Context, string) (map[string]any, error) { return nil, nil }
func (*flakyResolver) PutSecret(context.Context, string, string) error               { return nil }
func (*flakyResolver) ListSecrets(context.Context, string) ([]string, error)         { return nil, nil }
func (*flakyResolver) Close() error                                                  { return nil }

func TestClient_ResolveFailureIsNotCachedAndIsSanitized(t *testing.T) {
	res := &flakyResolver{fails: 1}
	p := NewProvider(fullSettings(), res, &http.Client{Transport: &hostAssertingTransport{t: t, body: comparison}})

	_, err := p.Client(context.Background())
	require.ErrorIs(t, err, ErrKeyUnavailable)
	assert.Contains(t, err.Error(), EnvKeySecret)
	assert.NotContains(t, err.Error(), testKey)

	c, err := p.Client(context.Background())
	require.NoError(t, err, "a failed build must be retried on the next call, not cached")
	require.NotNil(t, c)
	assert.EqualValues(t, 2, res.calls.Load())

	again, err := p.Client(context.Background())
	require.NoError(t, err)
	assert.Same(t, c, again)
	assert.EqualValues(t, 2, res.calls.Load(), "a built client is reused without resolving again")
}

func TestClient_InvalidOrgIDIsSanitized(t *testing.T) {
	s := fullSettings()
	s.OrgID = "not-a-uuid"
	_, err := NewProvider(s, envResolver(t), nil).Client(context.Background())
	require.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotContains(t, err.Error(), testKey)
}

// Production composition: env -> secret resolver -> lazy NewClient (hc injected
// only to replace the network) -> documented GET -> decode.
func TestComposition_EnvToDecodedComparison_AndRedaction(t *testing.T) {
	rt := &hostAssertingTransport{t: t, body: comparison}
	p := NewProvider(fullSettings(), envResolver(t), &http.Client{Transport: rt})

	assertNoKey := func(label string) {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(verb, p) + fmt.Sprintf(verb, struct{ P *Provider }{p}) + fmt.Sprintf(verb, fullSettings())
			assert.NotContains(t, out, testKey, "%s %s", label, verb)
		}
	}
	assertNoKey("before first comparison")

	c, err := p.Client(context.Background())
	require.NoError(t, err)
	got, err := c.Comparison(context.Background(), insurance.ComparisonRequest{PlanID: p.PlanID()})
	require.NoError(t, err)
	assert.Equal(t, 1, int(rt.calls.Load()))
	assert.Equal(t, "/v1/org/"+testOrg+"/commitment-plans/"+testPlan+"/comparison", rt.gotPath)
	assert.Equal(t, testKey, rt.gotKey)
	assert.Equal(t, testPlan, got.PlanID)

	assertNoKey("after a successful comparison (client now built)")
	assert.NotContains(t, fmt.Sprintf("%+v %v %#v", c, c, c), testKey)
}

// A secret stored with a trailing newline (common with files and CLI-created
// secrets) must still build a client: the library rejects non-printable key
// bytes, which would otherwise turn every comparison into ErrKeyUnavailable.
func TestClient_KeyWithTrailingNewlineIsTrimmed(t *testing.T) {
	t.Setenv(testSecret, testKey+"\n")
	rt := &hostAssertingTransport{t: t, body: comparison}
	p := NewProvider(fullSettings(), secrets.NewEnvResolver(), &http.Client{Transport: rt})

	c, err := p.Client(context.Background())
	require.NoError(t, err)
	_, err = c.Comparison(context.Background(), insurance.ComparisonRequest{PlanID: p.PlanID()})
	require.NoError(t, err)
	assert.Equal(t, 1, int(rt.calls.Load()))
}
