package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/accounts"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// POST /api/accounts/discover-org lists the organization with the org root's
// own credentials and persists every member as a cloud_accounts row, so a
// caller scoped to [A] must not be able to aim it at an org root outside
// their scope (issue #520).

// countingCredStore records credential loads so tests can prove an org root's
// credentials were never resolved.
type countingCredStore struct {
	*fakeCredStore
	loads int
}

func (c *countingCredStore) LoadRaw(ctx context.Context, accountID, credType string) ([]byte, error) {
	c.loads++
	return c.fakeCredStore.LoadRaw(ctx, accountID, credType)
}

type discoverOrgProbe struct {
	h       *Handler
	store   *MockConfigStore
	creds   *countingCredStore
	called  bool
	created []config.CloudAccount
}

func newDiscoverOrgProbe(t *testing.T, rootID string, scope ...string) *discoverOrgProbe {
	t.Helper()
	h, store := scopedHandler(t, scope...)
	p := &discoverOrgProbe{h: h, store: store}
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		root := orgRootAccount()
		root.ID = rootID
		if id != rootID {
			return nil, nil
		}
		return root, nil
	}
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, nil
	}
	store.CreateCloudAccountFn = func(_ context.Context, a *config.CloudAccount) error {
		p.created = append(p.created, *a)
		return nil
	}
	p.creds = &countingCredStore{fakeCredStore: &fakeCredStore{data: map[string][]byte{
		rootID + "::aws_access_keys": []byte(`{"access_key_id":"AKIATEST","secret_access_key":"shh"}`),
	}}}
	h.credStore = p.creds
	h.discoverOrgFn = func(_ context.Context, _ aws.Config) (*accounts.OrgDiscoveryResult, error) {
		p.called = true
		return &accounts.OrgDiscoveryResult{Accounts: []config.CloudAccount{
			{Provider: "aws", ExternalID: "300000000003", Name: "Member"},
		}}, nil
	}
	return p
}

func discoverOrgBody(rootID string) string {
	return `{"account_id":"` + rootID + `"}`
}

func TestRouterDispatch_DiscoverOrg_OutOfScopeRootRefused(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedOutAccount, scopedInAccount)

	_, err := NewRouter(p.h).Route(context.Background(), "POST", "/api/accounts/discover-org",
		scopedRequest(discoverOrgBody(scopedOutAccount)))

	require.Error(t, err)
	assert.True(t, IsNotFoundError(err), "expected the enumeration-safe not-found, got %v", err)
	assert.Zero(t, p.creds.loads, "the out-of-scope org root's credentials must not be loaded")
	assert.False(t, p.called, "organization listing must not run")
	assert.Empty(t, p.created, "no member rows may be persisted")
}

// A scoped admin whose scope contains the org root is still refused (issue
// #626): discovery would persist every organization member, not only the ones
// in the caller's scope, and the created/skipped counts are computed against
// every tenant's aws rows.
func TestRouterDispatch_DiscoverOrg_InScopeRootScopedCallerRefused(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedInAccount, scopedInAccount)

	_, err := NewRouter(p.h).Route(context.Background(), "POST", "/api/accounts/discover-org",
		scopedRequest(discoverOrgBody(scopedInAccount)))

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Zero(t, p.creds.loads, "the org root's credentials must not be loaded for a scoped caller")
	assert.False(t, p.called, "organization listing must not run for a scoped caller")
	assert.Empty(t, p.created, "no member rows may be persisted for a scoped caller")
}

func TestRouterDispatch_DiscoverOrg_UnrestrictedAdminUnchanged(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedOutAccount)

	res, err := NewRouter(p.h).Route(context.Background(), "POST", "/api/accounts/discover-org",
		scopedRequest(discoverOrgBody(scopedOutAccount)))

	require.NoError(t, err)
	dr, ok := res.(DiscoverOrgResult)
	require.True(t, ok, "result type = %T", res)
	assert.Equal(t, 1, dr.Created)
}

func TestRouterDispatch_DiscoverOrg_MissingRootSameErrorForScoped(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedOutAccount, scopedInAccount)
	missing := "33333333-3333-4333-8333-333333333333"

	_, errMissing := NewRouter(p.h).Route(context.Background(), "POST", "/api/accounts/discover-org",
		scopedRequest(discoverOrgBody(missing)))
	_, errOut := NewRouter(p.h).Route(context.Background(), "POST", "/api/accounts/discover-org",
		scopedRequest(discoverOrgBody(scopedOutAccount)))

	require.Error(t, errMissing)
	require.Error(t, errOut)
	assert.ErrorIs(t, errMissing, errNotFound)
	assert.ErrorIs(t, errOut, errNotFound)
	assert.Equal(t, errMissing, errOut, "missing and out-of-scope roots must be indistinguishable")
}

// discoverOrgViaHandleRequest drives POST /api/accounts/discover-org through
// the full HandleRequest pipeline (auth, routing, error mapping).
func discoverOrgViaHandleRequest(t *testing.T, p *discoverOrgProbe, rootID string) *events.LambdaFunctionURLResponse {
	t.Helper()
	mockAuth, ok := p.h.auth.(*MockAuthService)
	require.True(t, ok, "auth = %T", p.h.auth)
	mockAuth.On("ValidateCSRFToken", mock.Anything, scopedToken, "csrf-tok").Return(nil)
	resp, err := p.h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer " + scopedToken, "Content-Type": "application/json", "X-CSRF-Token": "csrf-tok"},
		Body:    discoverOrgBody(rootID),
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST", Path: "/api/accounts/discover-org"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

// Issue #626 regression through HandleRequest: a scoped admin aiming discovery
// at an in-scope org root gets a 403 and nothing is persisted, while an
// unrestricted admin still discovers and persists every member.
func TestHandleRequest_DiscoverOrg_ScopedAdminInScopeRootRefused(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedInAccount, scopedInAccount)

	resp := discoverOrgViaHandleRequest(t, p, scopedInAccount)

	assert.Equal(t, 403, resp.StatusCode, "body: %s", resp.Body)
	assert.Contains(t, resp.Body, "unrestricted account access")
	assert.NotContains(t, resp.Body, `"created"`, "no counts may be returned to a scoped caller")
	assert.Zero(t, p.creds.loads)
	assert.False(t, p.called)
	assert.Empty(t, p.created)
}

func TestHandleRequest_DiscoverOrg_UnrestrictedAdminPersistsAllMembers(t *testing.T) {
	p := newDiscoverOrgProbe(t, scopedOutAccount)

	resp := discoverOrgViaHandleRequest(t, p, scopedOutAccount)

	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
	var dr DiscoverOrgResult
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &dr))
	assert.Equal(t, DiscoverOrgResult{Discovered: 1, Created: 1}, dr)
	assert.True(t, p.called)
	require.Len(t, p.created, 1)
	assert.Equal(t, "300000000003", p.created[0].ExternalID)
	assert.False(t, p.created[0].Enabled)
	assert.Equal(t, scopedOutAccount, p.created[0].AWSBastionID)
}
