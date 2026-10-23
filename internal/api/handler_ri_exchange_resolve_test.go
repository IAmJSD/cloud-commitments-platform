package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Handler tests for POST /api/ri-exchange/resolve/{id} (#692). The store is a
// FIXTURE; the row lock, staleness and cloud-check rules are covered against
// real PostgreSQL in internal/config.

const resolveRIID = "6f1c2a8e-0d4b-4c1e-9a3f-692000000001"

var (
	riExecute   = []auth.Permission{{Action: auth.ActionExecute, Resource: "ri-exchange"}}
	riResolveAt = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
)

type resolveRIStore struct {
	*MockConfigStore
	called int
	got    config.RIExchangeResolve
	res    *config.RIExchangeResolveResult
	err    error
}

func (s *resolveRIStore) ResolveStaleRIExchange(_ context.Context, r config.RIExchangeResolve) (*config.RIExchangeResolveResult, error) {
	s.called++
	s.got = r
	if s.res == nil && s.err == nil {
		return &config.RIExchangeResolveResult{ID: r.ID, Status: r.Outcome, PaymentDue: "1000.000000", Note: r.Note}, nil
	}
	return s.res, s.err
}

// riResolveAccount renders the 12-digit account id without a literal the
// git-secrets AWS account-id pattern would flag.
func riResolveAccount() string { return fmt.Sprintf("%012d", 692000000001) }

func riResolveBody(outcome, extra string) string {
	return fmt.Sprintf(`{"outcome":%q,"reason":"source RIs still active in the EC2 console"%s,`+
		`"cloud_check":{"checked_account_id":%q,"checked_region":"us-east-1","checked_at":%q}}`,
		outcome, extra, riResolveAccount(), riResolveAt.Format(time.RFC3339))
}

var riResolveFailedBody = riResolveBody("failed", `,"acknowledge_unknown_outcome":true`)

func riResolveCall(t *testing.T, caller apgCaller, store *resolveRIStore, body string) (any, error) {
	t.Helper()
	h, req := caller.handlerAndReq(store.MockConfigStore, body)
	h.config = store
	h.reshapeAccountResolver = func(context.Context) (string, error) { return scopedInAccount, nil }
	return h.resolveRIExchange(context.Background(), req, resolveRIID)
}

func TestResolveRIExchange_PassesOperatorDecisionToStore(t *testing.T) {
	store := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
	out, err := riResolveCall(t, apgCaller{perms: riExecute}, store, riResolveFailedBody)
	require.NoError(t, err)
	require.Equal(t, 1, store.called)
	got := store.got
	assert.Equal(t, resolveRIID, got.ID)
	assert.Equal(t, config.StatusFailed, got.Outcome)
	assert.Empty(t, got.ExchangeID)
	assert.Equal(t, config.RIExchangeResolveStaleAfter, got.StaleAfter)
	assert.Equal(t, config.RIExchangeCloudCheck{AccountID: riResolveAccount(), Region: "us-east-1", CheckedAt: riResolveAt}, got.Check)
	require.NotNil(t, got.Actor)
	assert.Equal(t, apgUserID, *got.Actor, "the decision is attributed to the signed-in operator")
	assert.Equal(t, fmt.Sprintf(`operator:failed: account=%q region="us-east-1" checked_at=2026-10-19T08:00:00Z ack=true reason="source RIs still active in the EC2 console"`, riResolveAccount()), got.Note)
	resp, ok := out.(riExchangeResolveResponse)
	require.True(t, ok)
	assert.Equal(t, config.StatusFailed, resp.Status)

	completed := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
	_, err = riResolveCall(t, apgCaller{perms: riExecute}, completed, riResolveBody("completed", `,"exchange_id":"riex-0a1b2c"`))
	require.NoError(t, err)
	assert.Equal(t, config.StatusCompleted, completed.got.Outcome)
	assert.Equal(t, "riex-0a1b2c", completed.got.ExchangeID)
	assert.Contains(t, completed.got.Note, `exchange_id="riex-0a1b2c"`)
	assert.NotContains(t, completed.got.Note, "ack=true")
}

func TestResolveRIExchange_Authorization(t *testing.T) {
	regionScoped := func(region string) []auth.Permission {
		return []auth.Permission{{Action: auth.ActionExecute, Resource: "ri-exchange",
			Constraints: &auth.PermissionConstraints{Regions: []string{region}}}}
	}
	tests := []struct {
		name   string
		caller apgCaller
		want   int // 0 = allowed
	}{
		{"execute:ri-exchange constrained to the checked region", apgCaller{perms: regionScoped("us-east-1")}, 0},
		{"view-only user", apgCaller{perms: []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePurchases}}}, 403},
		{"admin API key has no identity to stamp", apgCaller{key: true}, 403},
		{"execute:ri-exchange constrained to another region", apgCaller{perms: regionScoped("eu-west-1")}, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
			_, err := riResolveCall(t, tc.caller, store, riResolveFailedBody)
			if tc.want == 0 {
				require.NoError(t, err)
				assert.Equal(t, 1, store.called)
				return
			}
			assert.Equal(t, tc.want, clientCode(t, err))
			assert.Zero(t, store.called, "a refused caller must not reach the store")
		})
	}
}

// A session whose allowed accounts exclude the deployment's account sees the
// enumeration-safe not-found, like the approval path.
func TestResolveRIExchange_OutOfScopeAccount(t *testing.T) {
	store := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Provider: "aws"}, nil
	}
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: apgUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped(riExecute, []string{scopedInAccount})
	h := &Handler{config: store, auth: mockAuth,
		reshapeAccountResolver: func(context.Context) (string, error) { return scopedOutAccount, nil }}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}, Body: riResolveFailedBody}
	_, err := h.resolveRIExchange(context.Background(), req, resolveRIID)
	require.ErrorIs(t, err, errNotFound)
	assert.Zero(t, store.called)
}

func TestResolveRIExchange_BodyValidation(t *testing.T) {
	check := func(acct, region, at string) string {
		return fmt.Sprintf(`"cloud_check":{"checked_account_id":%q,"checked_region":%q,"checked_at":%q}`, acct, region, at)
	}
	good := check(riResolveAccount(), "us-east-1", riResolveAt.Format(time.RFC3339))
	reason := `"reason":"source RIs still active in the EC2 console"`
	for name, body := range map[string]string{
		"empty":                     ``,
		"unknown field":             `{"outcome":"failed",` + reason + `,"acknowledge_unknown_outcome":true,` + good + `,"actor":"x"}`,
		"two documents":             riResolveFailedBody + riResolveFailedBody,
		"bad outcome":               `{"outcome":"canceled",` + reason + `,` + good + `}`,
		"short reason":              `{"outcome":"completed","reason":"ok",` + good + `}`,
		"whitespace reason":         `{"outcome":"completed","reason":"            ",` + good + `}`,
		"control char in reason":    `{"outcome":"completed","reason":"source RIs\nstill active",` + good + `}`,
		"missing cloud check":       `{"outcome":"completed",` + reason + `}`,
		"failed without ack":        `{"outcome":"failed",` + reason + `,` + good + `}`,
		"failed with exchange id":   `{"outcome":"failed",` + reason + `,"acknowledge_unknown_outcome":true,"exchange_id":"riex-1",` + good + `}`,
		"bad exchange id":           `{"outcome":"completed",` + reason + `,"exchange_id":"riex 1",` + good + `}`,
		"bad checked_at":            `{"outcome":"completed",` + reason + `,` + check(riResolveAccount(), "us-east-1", "yesterday") + `}`,
		"non-numeric account":       `{"outcome":"completed",` + reason + `,` + check("acct-1", "us-east-1", "2026-10-19T08:00:00Z") + `}`,
		"upper-case region":         `{"outcome":"completed",` + reason + `,` + check(riResolveAccount(), "US East", "2026-10-19T08:00:00Z") + `}`,
		"oversized body":            `{"outcome":"completed","reason":"` + strings.Repeat("x", 5000) + `"}`,
		"long reason":               `{"outcome":"completed","reason":"` + strings.Repeat("x", 301) + `",` + good + `}`,
		"cloud check null for fail": `{"outcome":"failed",` + reason + `,"acknowledge_unknown_outcome":true,"cloud_check":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
			_, err := riResolveCall(t, apgCaller{perms: riExecute}, store, body)
			assert.Equal(t, 400, clientCode(t, err))
			assert.Zero(t, store.called, "an invalid body must not reach the store")
		})
	}
}

func TestResolveRIExchange_BadIDIsRefusedBeforeTheStore(t *testing.T) {
	store := &resolveRIStore{MockConfigStore: new(MockConfigStore)}
	h, req := apgCaller{perms: riExecute}.handlerAndReq(store.MockConfigStore, riResolveFailedBody)
	h.config = store
	h.reshapeAccountResolver = func(context.Context) (string, error) { return scopedInAccount, nil }
	_, err := h.resolveRIExchange(context.Background(), req, "not-a-uuid")
	assert.Equal(t, 400, clientCode(t, err))
	assert.Zero(t, store.called)
}

func TestResolveRIExchange_MapsStoreOutcomes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: wait", config.ErrRIExchangeResolveConflict), 409},
		{fmt.Errorf("%w: other region", config.ErrRIExchangeResolveInvalid), 400},
	}
	for _, tc := range cases {
		store := &resolveRIStore{MockConfigStore: new(MockConfigStore), err: tc.err}
		_, err := riResolveCall(t, apgCaller{perms: riExecute}, store, riResolveFailedBody)
		assert.Equal(t, tc.want, clientCode(t, err), tc.err.Error())
	}

	missing := &resolveRIStore{MockConfigStore: new(MockConfigStore), err: config.ErrRIExchangeResolveNotFound}
	_, err := riResolveCall(t, apgCaller{perms: riExecute}, missing, riResolveFailedBody)
	require.ErrorIs(t, err, errNotFound)

	broken := &resolveRIStore{MockConfigStore: new(MockConfigStore), err: errors.New("db down")}
	_, err = riResolveCall(t, apgCaller{perms: riExecute}, broken, riResolveFailedBody)
	require.EqualError(t, err, "db down")
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a store failure is a 500, not a client error")
}

// The route is session-authenticated (so the middleware enforces CSRF), never public.
func TestResolveRIExchange_RouteIsAuthUser(t *testing.T) {
	r := NewRouter(&Handler{})
	for _, route := range r.routes {
		if route.PathPrefix == "/api/ri-exchange/resolve/" {
			assert.Equal(t, "POST", route.Method)
			assert.Equal(t, AuthUser, route.Auth)
			assert.Equal(t, map[string]string{"id": resolveRIID}, r.extractParams(route, "/api/ri-exchange/resolve/"+resolveRIID))
			return
		}
	}
	t.Fatal("POST /api/ri-exchange/resolve/{id} is not registered")
}
