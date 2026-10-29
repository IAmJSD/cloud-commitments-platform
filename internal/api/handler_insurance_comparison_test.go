package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/secrets"
)

const (
	cmpLineID = "22222222-2222-4222-8222-222222222222"
	cmpFin    = `{"commitment_cost":{"total":110,"breakdown":{"cloud_provider_cost":{"total":100},"archera_premium":10}},"commitment_savings":{"net":-5.5,"gross":4.5},"covered_ondemand_cost":104.5}`
	cmpOffer  = `{"is_current":true,"offer_id":"11111111-1111-4111-8111-111111111111","offer_org_id":"public","offer":{"provider":"aws","type":"aws/AmazonEC2","region":"us-east-1","guaranteed_display_name":null},"lease_menu_item_id":null,"selected_amount":3,"commitment_type":"aws/AmazonEC2","contract_term":null,"payment_option":null,"discount_rate":0.3,"breakeven_days":null,"commitment_upfront_cost":1200,"commitment_financials_monthly_rate":` + cmpFin + `,"delta_vs_current":{"monthly_net_savings":0,"upfront_cost":0,"discount_rate":0,"breakeven_days":null}}`
	cmpBody   = `{"current_totals":{"commitment_financials_monthly_rate":` + cmpFin + `,"commitment_upfront_cost":1200},"hypothetical_totals":[],"data":[{"line_item_id":"` + cmpLineID + `","current":` + cmpOffer + `,"candidates":[]}]}`
)

type vendorTransport struct {
	t       *testing.T
	calls   int
	status  int
	body    string
	headers http.Header
	lastReq *http.Request
}

func (v *vendorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	v.calls++
	v.lastReq = r
	assert.Equal(v.t, "https", r.URL.Scheme)
	assert.Equal(v.t, "api.archera.ai", r.URL.Host)
	h := v.headers
	if h == nil {
		h = http.Header{}
	}
	code := v.status
	if code == 0 {
		code = 200
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(v.body)), Request: r}, nil
}

func adminCtx() context.Context {
	return contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalAdminAPIKey})
}

func cmpHandler(t *testing.T, s archera.Settings, vt *vendorTransport) *Handler {
	t.Helper()
	p := archera.NewProvider(s, secrets.NewEnvResolver(), &http.Client{Transport: vt})
	return NewHandler(HandlerConfig{Insurance: p})
}

func completeEnv(t *testing.T) archera.Settings {
	t.Helper()
	t.Setenv(archera.EnvOrgID, insTestOrg)
	t.Setenv(archera.EnvPlanID, insTestPlan)
	t.Setenv(archera.EnvKeySecret, insTestRef)
	t.Setenv(insTestRef, insTestKey)
	return archera.SettingsFromEnv()
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := logging.SetOutput(&buf)
	logging.SetLevel("debug")
	t.Cleanup(func() { logging.SetOutput(prev) })
	return &buf
}

// Production composition: env -> SettingsFromEnv -> env secret resolver ->
// lazy insurance.NewClient (only the RoundTripper replaces the network) ->
// router -> handler -> JSON DTO.
func TestInsuranceComparison_Composition(t *testing.T) {
	vt := &vendorTransport{t: t, body: cmpBody}
	h := cmpHandler(t, completeEnv(t), vt)

	res, err := NewRouter(h).Route(adminCtx(), "GET", "/api/insurance/comparison", &events.LambdaFunctionURLRequest{})
	require.NoError(t, err)

	require.Equal(t, 1, vt.calls)
	assert.Equal(t, http.MethodGet, vt.lastReq.Method)
	assert.Equal(t, "/v1/org/"+insTestOrg+"/commitment-plans/"+insTestPlan+"/comparison", vt.lastReq.URL.Path)
	assert.Equal(t, insTestKey, vt.lastReq.Header.Get("x-api-key"))
	assert.Empty(t, vt.lastReq.URL.RawQuery, "no filters are passed")

	dto, ok := res.(*archera.ComparisonDTO)
	require.True(t, ok)
	assert.Equal(t, insTestPlan, dto.PlanID)
	assert.True(t, dto.PremiumIncluded)
	assert.Equal(t, "supported", dto.Rows[0].Current.ProductSupport.Status)
	assert.Equal(t, "110", *dto.Current.CommitmentCostTotal)
}

// distinctBody has a different value in every numeric vendor field, so a
// mapping that swaps or drops one cannot pass the composition test.
func distinctBody() string {
	fin := func(total, cloud, prem, net, gross, covered string) string {
		return `{"commitment_cost":{"total":` + total + `,"breakdown":{"cloud_provider_cost":{"total":` + cloud + `},"archera_premium":` + prem + `}},"commitment_savings":{"net":` + net + `,"gross":` + gross + `},"covered_ondemand_cost":` + covered + `}`
	}
	offer := `{"is_current":true,"offer_id":"11111111-1111-4111-8111-111111111111","offer_org_id":"public","offer":{"provider":"aws","type":"aws/AmazonEC2","region":"us-east-1","guaranteed_display_name":null},"lease_menu_item_id":null,"selected_amount":3,"commitment_type":"aws/AmazonEC2","contract_term":"one_year_gris","payment_option":"no_upfront","discount_rate":0.18,"breakeven_days":19,"commitment_upfront_cost":17,"commitment_financials_monthly_rate":` + fin("31", "32", "33", "35", "34", "36") + `,"delta_vs_current":{"monthly_net_savings":21,"upfront_cost":22,"discount_rate":0.23,"breakeven_days":24}}`
	return `{"current_totals":{"commitment_financials_monthly_rate":` + fin("11", "12", "13", "15", "14", "16") + `,"commitment_upfront_cost":10},"hypothetical_totals":[],"data":[{"line_item_id":"` + cmpLineID + `","current":` + offer + `,"candidates":[]}]}`
}

func TestInsuranceComparison_DistinctValuesReachTheirOwnFields(t *testing.T) {
	vt := &vendorTransport{t: t, body: distinctBody()}
	h := cmpHandler(t, completeEnv(t), vt)
	dto, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
	require.NoError(t, err)

	c := dto.Current
	assert.Equal(t, []string{"11", "12", "13", "14", "15", "16", "10"}, []string{
		*c.CommitmentCostTotal, *c.CloudProviderCost, *c.Premium, *c.GrossSavings, *c.NetSavings, *c.CoveredOnDemandCost, *c.UpfrontCost})
	o := dto.Rows[0].Current
	m := o.Monthly
	assert.Equal(t, []string{"31", "32", "33", "34", "35", "36", "17", "0.18", "19"}, []string{
		*m.CommitmentCostTotal, *m.CloudProviderCost, *m.Premium, *m.GrossSavings, *m.NetSavings, *m.CoveredOnDemandCost,
		*o.UpfrontCost, *o.DiscountRate, *o.BreakevenDays})
	d := o.DeltaVsCurrent
	assert.Equal(t, []string{"21", "22", "0.23", "24"}, []string{*d.MonthlyNetSavings, *d.UpfrontCost, *d.DiscountRate, *d.BreakevenDays})
	assert.Equal(t, "one_year_gris", *o.ContractTerm)
	assert.Equal(t, "no_upfront", *o.PaymentOption)
	assert.True(t, o.IsCurrent)
	assert.Equal(t, "aws", o.Provider)
}

func TestInsuranceComparison_ProductionPassesNilHTTPClient(t *testing.T) {
	// app.go must build the provider with hc=nil so the SSRF-hardened default
	// client is used; only tests inject a transport.
	src, err := os.ReadFile("../server/app.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "archera.NewProvider(archera.SettingsFromEnv(), deps.SecretResolver, nil)")
}

func TestInsuranceComparison_NotConfiguredMakesNoRequest(t *testing.T) {
	vt := &vendorTransport{t: t}
	for name, s := range map[string]archera.Settings{
		"off":        {},
		"incomplete": {OrgID: insTestOrg, KeySecretRef: insTestRef},
	} {
		t.Run(name, func(t *testing.T) {
			h := cmpHandler(t, s, vt)
			_, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
			ce, ok := IsClientError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 503, ce.code)
			for _, missing := range s.Missing() {
				assert.Contains(t, ce.message, missing)
			}
			assert.NotContains(t, ce.message, insTestOrg)
			assert.Zero(t, vt.calls)
		})
	}
	h := &Handler{insurance: nil}
	_, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
	ce, _ := IsClientError(err)
	require.NotNil(t, ce)
	assert.Equal(t, 503, ce.code)
}

func TestInsuranceComparison_KeyUnavailableNamesSetting(t *testing.T) {
	t.Setenv(archera.EnvOrgID, insTestOrg)
	t.Setenv(archera.EnvPlanID, insTestPlan)
	t.Setenv(archera.EnvKeySecret, "SYNTHETIC_UNSET_REF")
	vt := &vendorTransport{t: t}
	h := cmpHandler(t, archera.SettingsFromEnv(), vt)
	_, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 503, ce.code)
	assert.Contains(t, ce.message, archera.EnvKeySecret)
	assert.Zero(t, vt.calls)
}

func TestInsuranceComparison_VendorErrorsAreSanitized(t *testing.T) {
	fragment := insTestKey[:12]
	echo := `{"message":"bad key ` + insTestKey + ` fragment ` + fragment + `"}`
	cases := []struct {
		name    string
		status  int
		body    string
		headers http.Header
		code    int
		retry   string
	}{
		{"401 echoing key", 401, echo, nil, 502, ""},
		{"403", 403, echo, nil, 502, ""},
		{"404", 404, echo, nil, 502, ""},
		{"429 with Retry-After", 429, echo, http.Header{"Retry-After": {"120"}}, 429, "120"},
		{"429 without Retry-After", 429, echo, nil, 429, ""},
		{"429 zero Retry-After", 429, echo, http.Header{"Retry-After": {"0"}}, 429, ""},
		{"500", 500, echo, nil, 502, ""},
		{"oversized body", 200, strings.Repeat(" ", 9<<20), nil, 502, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			vt := &vendorTransport{t: t, status: tc.status, body: tc.body, headers: tc.headers}
			h := cmpHandler(t, completeEnv(t), vt)
			_, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
			ce, ok := IsClientError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.code, ce.code)
			for _, leak := range []string{insTestKey, fragment, "bad key"} {
				assert.NotContains(t, ce.message, leak)
				assert.NotContains(t, logs.String(), leak)
			}
			if tc.retry == "" {
				assert.Empty(t, ce.headers)
				assert.NotContains(t, ce.Details(), "retry_after_seconds")
			} else {
				assert.Equal(t, tc.retry, ce.headers["Retry-After"])
				assert.Contains(t, ce.Details(), "retry_after_seconds")
			}
		})
	}
}

func TestInsuranceComparison_RetryAfterHeaderReachesResponse(t *testing.T) {
	h := &Handler{}
	err := NewClientErrorWithHeaders(429, "x", map[string]any{"retry_after_seconds": 7}, map[string]string{"Retry-After": "7"})
	base := map[string]string{"Access-Control-Allow-Origin": "o"}
	status, body := h.handleRequestError(err)
	resp := h.buildResponse(status, errorResponseHeaders(err, base), body, nil)
	assert.Equal(t, "7", resp.Headers["Retry-After"])
	assert.Equal(t, "o", resp.Headers["Access-Control-Allow-Origin"])
	assert.Equal(t, 429, resp.StatusCode)
	assert.Contains(t, resp.Body, `"retry_after_seconds":7`)
	assert.NotContains(t, base, "Retry-After", "the base header map is not mutated")
}

func TestInsuranceComparison_TooLargeFailsLoud(t *testing.T) {
	// 9000 synthetic rows, each ~190 bytes of DTO JSON, exceed 5 MiB
	// while the vendor body stays under the 8 MiB client bound.
	var rows []string
	for i := 0; i < 9000; i++ {
		rows = append(rows, `{"line_item_id":"`+cmpLineID+`","current":`+cmpOffer+`,"candidates":[]}`)
	}
	body := `{"current_totals":{"commitment_financials_monthly_rate":` + cmpFin + `,"commitment_upfront_cost":1200},"hypothetical_totals":[],"data":[` + strings.Join(rows, ",") + `]}`
	require.Less(t, len(body), 8<<20, "vendor body must stay under the client bound for this test")
	vt := &vendorTransport{t: t, body: body}
	h := cmpHandler(t, completeEnv(t), vt)
	_, err := h.getInsuranceComparison(adminCtx(), &events.LambdaFunctionURLRequest{})
	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 502, ce.code)
	assert.Equal(t, "comparison too large to return", ce.message)
}

func TestInsuranceComparison_DeniedAndScoped(t *testing.T) {
	vt := &vendorTransport{t: t, body: cmpBody}
	s := completeEnv(t)
	p := archera.NewProvider(s, secrets.NewEnvResolver(), &http.Client{Transport: vt})

	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasPermissionAPI", mock.Anything, "u1", "view", "recommendations").Return(false, nil)
	_, err := (&Handler{auth: m, insurance: p}).getInsuranceComparison(sessionPrincipalCtx(), &events.LambdaFunctionURLRequest{})
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 403, ce.code)

	m2 := new(MockAuthService)
	t.Cleanup(func() { m2.AssertExpectations(t) })
	m2.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "view", "recommendations").Return("u2", "key-1", true, nil)
	m2.On("GetAllowedAccountsAPI", mock.Anything, "u2").Return([]string{"acct-1"}, nil)
	ctx := contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalUserAPIKey, UserID: "u2", APIKeyID: "key-1"})
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "user-key"}}
	_, err = (&Handler{auth: m2, insurance: p}).getInsuranceComparison(ctx, req)
	assert.ErrorIs(t, err, errNotFound)
	assert.Zero(t, vt.calls, "a denied or scoped caller must not cause an outbound request")
}

// Through the real request path (HandleRequest -> router -> handler ->
// response), so removing the header plumbing in executeRequest fails.
func TestInsuranceComparison_RealRequestPath_RetryAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		headers http.Header
		want    string
	}{
		"positive Retry-After": {http.Header{"Retry-After": {"120"}}, "120"},
		"no Retry-After":       {nil, ""},
		"zero Retry-After":     {http.Header{"Retry-After": {"0"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			vt := &vendorTransport{t: t, status: 429, body: `{"message":"slow down"}`, headers: tc.headers}
			h := cmpHandler(t, completeEnv(t), vt)
			h.apiKey = "admin-key"
			req := &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"x-api-key": "admin-key"},
				RequestContext: events.LambdaFunctionURLRequestContext{
					HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/insurance/comparison"},
				},
			}
			resp, err := h.HandleRequest(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, 429, resp.StatusCode)
			require.Equal(t, 1, vt.calls)
			if tc.want == "" {
				assert.NotContains(t, resp.Headers, "Retry-After")
				assert.NotContains(t, resp.Body, "retry_after_seconds")
				return
			}
			assert.Equal(t, tc.want, resp.Headers["Retry-After"])
			assert.Contains(t, resp.Body, `"retry_after_seconds":`+tc.want)
		})
	}
}

func TestMapInsuranceError_PinsEachStatusMessage(t *testing.T) {
	for status, want := range map[int]struct {
		code int
		msg  string
	}{
		401: {502, "Archera rejected the configured credentials"},
		403: {502, "Archera rejected the configured credentials"},
		404: {502, "Archera organization or plan was not found"},
		500: {502, "Archera service error"},
		503: {502, "Archera service error"},
		418: {502, "Archera request failed with HTTP 418"},
	} {
		ce, ok := IsClientError(mapInsuranceError(&insurance.HTTPError{StatusCode: status, Message: "vendor text"}))
		require.True(t, ok)
		assert.Equal(t, want.code, ce.code, status)
		assert.Equal(t, want.msg, ce.message, status)
	}
	ce, _ := IsClientError(mapInsuranceError(errors.New("boom")))
	assert.Equal(t, "Archera comparison failed", ce.message)
	ce, _ = IsClientError(mapInsuranceError(&insurance.HTTPError{StatusCode: 429}))
	assert.Equal(t, "Archera rate limit reached; retry-after not given", ce.message)
}

// ctxBlockedResolver parks GetSecret until the caller's ctx ends.
type ctxBlockedResolver struct{ secrets.Resolver }

func (ctxBlockedResolver) GetSecret(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestInsuranceComparison_EndedKeyBuildIsTheDeadlineStatus(t *testing.T) {
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(adminCtx(), 20*time.Millisecond)
		},
		"canceled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(adminCtx())
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			s := completeEnv(t)
			vt := &vendorTransport{t: t}
			p := archera.NewProvider(s, ctxBlockedResolver{secrets.NewEnvResolver()}, &http.Client{Transport: vt})
			h := NewHandler(HandlerConfig{Insurance: p})
			ctx, cancel := mk()
			defer cancel()
			_, err := h.getInsuranceComparison(ctx, &events.LambdaFunctionURLRequest{})
			ce, ok := IsClientError(err)
			require.True(t, ok)
			assert.Equal(t, 504, ce.code)
			assert.Equal(t, insuranceTimeoutMessage, ce.message)
			assert.NotContains(t, logs.String(), "credential resolution failed")
			assert.Zero(t, vt.calls)
		})
	}
}

// ctxBlockedTransport parks the vendor call until the request's ctx ends. With
// inBody it answers 200 at once and parks the body read instead, so the ctx
// error surfaces from the decode rather than from http.Client.Do.
type ctxBlockedTransport struct {
	inBody bool
	calls  int
}

type ctxBlockedBody struct{ ctx context.Context }

func (b ctxBlockedBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (ctxBlockedBody) Close() error { return nil }

func (c *ctxBlockedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	if c.inBody {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: ctxBlockedBody{r.Context()}, Request: r}, nil
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// The key resolves and the client builds; the request's ctx then ends during
// the comparison call itself. That is the same deadline status as an ended key
// build (issue #1498), not a 502 vendor failure.
func TestInsuranceComparison_EndedVendorCallIsTheDeadlineStatus(t *testing.T) {
	ends := map[string]func() (context.Context, context.CancelFunc){
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(adminCtx(), 50*time.Millisecond)
		},
		"canceled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(adminCtx())
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		},
	}
	for endName, mk := range ends {
		for _, inBody := range []bool{false, true} {
			name := endName + "/during request"
			if inBody {
				name = endName + "/during body read"
			}
			t.Run(name, func(t *testing.T) {
				logs := captureLogs(t)
				ct := &ctxBlockedTransport{inBody: inBody}
				p := archera.NewProvider(completeEnv(t), secrets.NewEnvResolver(), &http.Client{Transport: ct})
				h := NewHandler(HandlerConfig{Insurance: p})
				ctx, cancel := mk()
				defer cancel()
				_, err := h.getInsuranceComparison(ctx, &events.LambdaFunctionURLRequest{})
				ce, ok := IsClientError(err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, 504, ce.code)
				assert.Equal(t, insuranceTimeoutMessage, ce.message)
				assert.Equal(t, 1, ct.calls, "the client was built and the vendor call was made")
				assert.NotContains(t, logs.String(), "archera comparison failed")
			})
		}
	}
}
