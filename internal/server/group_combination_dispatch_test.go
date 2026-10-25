package server

// End-to-end dispatch coverage for the issue #78 group-combination check: a
// real HTTP request through Handler.HandleRequest -> Router.Route ->
// requireAdmin -> createUser/updateUser -> the REAL auth.Service, with only the
// store mocked. Reuses the #1756 harness (newSelfScopeHarness), whose actor is
// an administrator over acct-A.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// comboDispatchViewers is view-only and inside the actor's account scope, so
// the access guards pass and only the combination check can refuse it.
var comboDispatchViewers = &auth.Group{
	ID:              "55555555-5555-4555-8555-555555555558",
	Name:            "Acct-A Viewers",
	Permissions:     []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}},
	AllowedAccounts: []string{scopeDispatchAccountID},
}

func usersRequest(t *testing.T, method, path string, body map[string]any) *events.LambdaFunctionURLRequest {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := selfMembershipRequest(t, nil)
	req.Body = string(raw)
	req.RequestContext.HTTP.Method = method
	req.RequestContext.HTTP.Path = path
	return req
}

func TestGroupCombinationDispatch_UpdateRefusedWith400(t *testing.T) {
	handler, mockStore := newSelfScopeHarness(t, []string{scopeDispatchGroupID})
	mockStore.On("GetGroup", mock.Anything, comboDispatchViewers.ID).Return(comboDispatchViewers, nil)

	resp, err := handler.HandleRequest(context.Background(),
		selfMembershipRequest(t, []string{scopeDispatchGroupID, comboDispatchViewers.ID}))

	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode, "body: %s", resp.Body)
	assert.Contains(t, resp.Body, "contradictory group combination")
	assert.Contains(t, resp.Body, "Acct-A Viewers")
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestGroupCombinationDispatch_CreateRefusedWith400(t *testing.T) {
	handler, mockStore := newSelfScopeHarness(t, []string{scopeDispatchGroupID})
	mockStore.On("GetGroup", mock.Anything, comboDispatchViewers.ID).Return(comboDispatchViewers, nil)
	mockStore.On("GetUserByEmail", mock.Anything, "new-user@example.com").Return(nil, nil)

	resp, err := handler.HandleRequest(context.Background(), usersRequest(t, "POST", "/api/users", map[string]any{
		"email":  "new-user@example.com",
		"groups": []string{comboDispatchViewers.ID, scopeDispatchGroupID},
	}))

	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode, "body: %s", resp.Body)
	assert.Contains(t, resp.Body, "contradictory group combination")
	mockStore.AssertNotCalled(t, "CreateUser", mock.Anything, mock.Anything)
}

// A user who already holds a contradictory membership can still be renamed
// through the route, with the groups omitted or echoed back unchanged.
func TestGroupCombinationDispatch_LegacyUserRenameAllowed(t *testing.T) {
	legacy := []string{scopeDispatchGroupID, comboDispatchViewers.ID}
	for name, body := range map[string]map[string]any{
		"groups omitted":          {"email": "renamed@example.com"},
		"groups echoed unchanged": {"email": "renamed@example.com", "groups": []string{comboDispatchViewers.ID, scopeDispatchGroupID}},
	} {
		t.Run(name, func(t *testing.T) {
			handler, mockStore := newSelfScopeHarness(t, legacy)
			mockStore.On("GetGroup", mock.Anything, comboDispatchViewers.ID).Return(comboDispatchViewers, nil).Maybe()
			mockStore.On("GetUserByEmail", mock.Anything, "renamed@example.com").Return(nil, nil).Once()
			mockStore.On("UpdateUserAdminFields", mock.Anything, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

			resp, err := handler.HandleRequest(context.Background(),
				usersRequest(t, "PUT", "/api/users/"+scopeDispatchUserID, body))

			require.NoError(t, err)
			assert.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
		})
	}
}
