package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/secrets"
)

const (
	insTestKey  = "synthetic-archera-key-0123456789"
	insTestOrg  = "11111111-1111-4111-8111-111111111111"
	insTestPlan = "44444444-4444-4444-8444-444444444444"
	insTestRef  = "SYNTHETIC_ARCHERA_KEY_REF"
)

type panicTransport struct{ t *testing.T }

func (p panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.t.Fatal("status must make no outbound request")
	return nil, nil
}

func insuranceProvider(t *testing.T, s archera.Settings) *archera.Provider {
	return archera.NewProvider(s, nil, &http.Client{Transport: panicTransport{t}})
}

func sessionPrincipalCtx() context.Context {
	const userID = "u1"
	return contextWithPrincipal(context.Background(), &Principal{
		Kind: PrincipalSession, Session: &Session{UserID: userID}, UserID: userID,
	})
}

func TestInsuranceStatus_AdminAPIKey_ReportsPresenceOnly(t *testing.T) {
	full := archera.Settings{OrgID: insTestOrg, PlanID: insTestPlan, KeySecretRef: insTestRef}
	for name, tc := range map[string]struct {
		s       archera.Settings
		ok      bool
		missing []string
	}{
		"off":      {archera.Settings{}, false, []string{archera.EnvKeySecret, archera.EnvOrgID, archera.EnvPlanID}},
		"complete": {full, true, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{apiKey: "admin-key", insurance: insuranceProvider(t, tc.s)}
			req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "admin-key"}}
			st, err := h.getInsuranceStatus(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tc.ok, st.Configured)
			assert.Equal(t, tc.missing, st.Missing)
			assert.NotContains(t, fmt.Sprintf("%+v", st), insTestOrg, "status must never carry IDs")
		})
	}
}

func TestInsuranceStatus_UnwiredIsOff(t *testing.T) {
	h := &Handler{apiKey: "admin-key"}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "admin-key"}}
	st, err := h.getInsuranceStatus(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, st.Configured)
}

func TestInsuranceStatus_DeniedUserGets403(t *testing.T) {
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasPermissionAPI", mock.Anything, "u1", "view", "recommendations").Return(false, nil)
	h := &Handler{auth: m, insurance: insuranceProvider(t, archera.Settings{})}
	_, err := h.getInsuranceStatus(sessionPrincipalCtx(), &events.LambdaFunctionURLRequest{})
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 403, ce.code)
}

func TestInsuranceStatus_ScopedSessionGets404(t *testing.T) {
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasPermissionAPI", mock.Anything, "u1", "view", "recommendations").Return(true, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, "u1").Return([]string{"acct-1"}, nil)
	h := &Handler{auth: m, insurance: insuranceProvider(t, archera.Settings{})}
	_, err := h.getInsuranceStatus(sessionPrincipalCtx(), &events.LambdaFunctionURLRequest{})
	assert.ErrorIs(t, err, errNotFound)
}

func TestInsuranceStatus_UnrestrictedSessionAllowed(t *testing.T) {
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasPermissionAPI", mock.Anything, "u1", "view", "recommendations").Return(true, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, "u1").Return([]string(nil), nil)
	h := &Handler{auth: m, insurance: insuranceProvider(t, archera.Settings{})}
	st, err := h.getInsuranceStatus(sessionPrincipalCtx(), &events.LambdaFunctionURLRequest{})
	require.NoError(t, err)
	assert.False(t, st.Configured)
}

func TestInsuranceStatus_ScopedUserAPIKeyGets404(t *testing.T) {
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "view", "recommendations").Return("u2", "key-1", true, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, "u2").Return([]string{"acct-1"}, nil)
	h := &Handler{auth: m, insurance: insuranceProvider(t, archera.Settings{})}
	ctx := contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalUserAPIKey, UserID: "u2", APIKeyID: "key-1"})
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "user-key"}}
	_, err := h.getInsuranceStatus(ctx, req)
	assert.ErrorIs(t, err, errNotFound)
}

func TestInsuranceStatus_RouteIsAuthenticated(t *testing.T) {
	h := &Handler{auth: new(MockAuthService), insurance: insuranceProvider(t, archera.Settings{})}
	_, err := NewRouter(h).Route(context.Background(), "GET", "/api/insurance/status", &events.LambdaFunctionURLRequest{})
	require.Error(t, err, "an unauthenticated request must not reach the handler")
}

// The Handler holds a lazily built *insurance.Client behind the provider; no
// fmt verb applied to the Handler may print the key.
func TestInsurance_HandlerFormattingNeverPrintsKey(t *testing.T) {
	t.Setenv(insTestRef, insTestKey)
	p := archera.NewProvider(archera.Settings{OrgID: insTestOrg, PlanID: insTestPlan, KeySecretRef: insTestRef}, secrets.NewEnvResolver(), nil)
	h := &Handler{insurance: p}
	check := func(label string) {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			assert.NotContains(t, fmt.Sprintf(verb, h), insTestKey, label+" "+verb)
		}
	}
	check("before the client is built")
	_, err := p.Client(context.Background())
	require.NoError(t, err)
	check("after the client is built")
}
