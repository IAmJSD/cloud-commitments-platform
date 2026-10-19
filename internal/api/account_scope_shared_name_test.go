package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #433: display names are not unique, so a scope entry that names an
// account must not also grant every other account with the same name (in
// another provider or tenant, or one registered later).
//
// Issue #1034: a name entry is not bound to any account either, so once the
// account it was meant for is deleted or renamed it would grant whichever
// account takes the name next. Scope entries therefore match by account ID
// only; a name entry grants nothing, unique or not.

var (
	sharedNameAWS   = config.CloudAccount{ID: "a1a1a1a1-0000-4000-8000-000000000001", Name: "prod", Provider: "aws", ExternalID: "111", Enabled: true}
	sharedNameAzure = config.CloudAccount{ID: "a2a2a2a2-0000-4000-8000-000000000002", Name: "prod", Provider: "azure", ExternalID: "sub-1", Enabled: true}
	sharedNameSolo  = config.CloudAccount{ID: "a3a3a3a3-0000-4000-8000-000000000003", Name: "solo", Provider: "gcp", ExternalID: "proj-1", Enabled: true}
)

// sharedNameHandler wires a session user whose allow-list is scope against a
// store holding accounts. listErr makes ListCloudAccounts fail.
func sharedNameHandler(t *testing.T, scope []string, accounts []config.CloudAccount, listErr error) *Handler {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		if listErr != nil {
			return nil, listErr
		}
		return append([]config.CloudAccount(nil), accounts...), nil
	}
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		for i := range accounts {
			if accounts[i].ID == id {
				a := accounts[i]
				return &a, nil
			}
		}
		return nil, nil
	}
	h := &Handler{auth: mockAuth, config: store}
	h.sourceIdentityOnce.Do(func() {}) // no STS lookup from markSelfAccount
	return h
}

func sharedNameGET(t *testing.T, h *Handler, path string) *events.LambdaFunctionURLResponse {
	t.Helper()
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: path},
		},
	})
	require.NoError(t, err)
	return resp
}

// listedIDs returns the account IDs GET /api/accounts shows the session.
func listedIDs(t *testing.T, h *Handler) []string {
	t.Helper()
	resp := sharedNameGET(t, h, "/api/accounts")
	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
	var accts []config.CloudAccount
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &accts))
	ids := make([]string, 0, len(accts))
	for i := range accts {
		ids = append(ids, accts[i].ID)
	}
	sort.Strings(ids)
	return ids
}

func TestHandleRequest_AccountScope_SharedDisplayName(t *testing.T) {
	t.Parallel()
	registered := []config.CloudAccount{sharedNameAWS, sharedNameAzure, sharedNameSolo}

	for _, tc := range []struct {
		name      string
		scope     []string
		wantList  []string
		reachable map[string]bool // GET /api/accounts/{id} -> 200 (true) or 404
	}{
		{
			name:     "shared name grants neither same-named account",
			scope:    []string{"prod"},
			wantList: []string{},
			reachable: map[string]bool{
				sharedNameAWS.ID: false, sharedNameAzure.ID: false, sharedNameSolo.ID: false,
			},
		},
		{
			name:     "ID entry still grants its account despite the shared name",
			scope:    []string{sharedNameAWS.ID},
			wantList: []string{sharedNameAWS.ID},
			reachable: map[string]bool{
				sharedNameAWS.ID: true, sharedNameAzure.ID: false, sharedNameSolo.ID: false,
			},
		},
		{
			name:     "shared name plus an ID entry grants only the ID's account",
			scope:    []string{"prod", sharedNameAzure.ID},
			wantList: []string{sharedNameAzure.ID},
			reachable: map[string]bool{
				sharedNameAWS.ID: false, sharedNameAzure.ID: true, sharedNameSolo.ID: false,
			},
		},
		{
			name:     "a unique name entry grants nothing either",
			scope:    []string{"solo"},
			wantList: []string{},
			reachable: map[string]bool{
				sharedNameAWS.ID: false, sharedNameAzure.ID: false, sharedNameSolo.ID: false,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := sharedNameHandler(t, tc.scope, registered, nil)
			assert.Equal(t, tc.wantList, listedIDs(t, h), "GET /api/accounts")
			for id, want := range tc.reachable {
				resp := sharedNameGET(t, h, "/api/accounts/"+id)
				if want {
					assert.Equal(t, 200, resp.StatusCode, "GET %s: %s", id, resp.Body)
				} else {
					assert.Equal(t, 404, resp.StatusCode, "GET %s: %s", id, resp.Body)
				}
			}
		})
	}
}

// Issue #1034: the account a name entry was meant for is deleted, and a new
// account is registered under the same name. The new account is the only one
// carrying the name, so a name match would grant it; it must not.
func TestHandleRequest_AccountScope_NameReusedAfterDelete(t *testing.T) {
	t.Parallel()
	reuser := sharedNameAzure // registered after sharedNameAWS ("prod") was deleted
	h := sharedNameHandler(t, []string{"prod"}, []config.CloudAccount{reuser, sharedNameSolo}, nil)

	assert.Equal(t, 404, sharedNameGET(t, h, "/api/accounts/"+reuser.ID).StatusCode,
		"an account that took a deleted account's name must not inherit its grant")
	assert.Empty(t, listedIDs(t, h))
}

// Issue #1034: the granted account is renamed and another account takes its
// old name. A grant stored as the account's ID stays with the renamed account
// and does not move to the newcomer.
func TestHandleRequest_AccountScope_IDGrantSurvivesRename(t *testing.T) {
	t.Parallel()
	renamed := sharedNameAWS
	renamed.Name = "prod-legacy"
	newcomer := sharedNameAzure // now the only account called "prod"
	h := sharedNameHandler(t, []string{sharedNameAWS.ID}, []config.CloudAccount{renamed, newcomer}, nil)

	assert.Equal(t, 200, sharedNameGET(t, h, "/api/accounts/"+renamed.ID).StatusCode)
	assert.Equal(t, 404, sharedNameGET(t, h, "/api/accounts/"+newcomer.ID).StatusCode)
	assert.Equal(t, []string{renamed.ID}, listedIDs(t, h))
}

// A row known only by external ID is identified through the name of the
// account carrying it. A disabled row still carries its name, so it still
// makes the name ambiguous (#433) and the row fails closed.
func TestHandleRequest_AccountScope_DisabledDuplicateCounts(t *testing.T) {
	t.Parallel()
	disabled := sharedNameAzure
	disabled.Enabled = false
	legacy := []config.PurchaseHistoryRecord{{PurchaseID: "legacy", AccountID: sharedNameAWS.ExternalID, Status: "completed"}}
	scope := []string{sharedNameAWS.ID}

	assert.Equal(t, []string{"legacy"}, historyScopeIDs(t, scope, []config.CloudAccount{sharedNameAWS}, legacy),
		"control: a unique name identifies the in-scope account")
	assert.Empty(t, historyScopeIDs(t, scope, []config.CloudAccount{sharedNameAWS, disabled}, legacy),
		"a disabled same-named account makes the name ambiguous")
}

// When the account list cannot be loaded, names cannot be resolved. Name
// entries match nothing in any case, and ID entries keep working.
func TestGetAccountScope_AccountListFailureIsIDOnly(t *testing.T) {
	t.Parallel()
	registered := []config.CloudAccount{sharedNameSolo}
	boom := errors.New("db down")

	byName := sharedNameHandler(t, []string{"solo"}, registered, boom)
	assert.Equal(t, 404, sharedNameGET(t, byName, "/api/accounts/"+sharedNameSolo.ID).StatusCode)

	byID := sharedNameHandler(t, []string{sharedNameSolo.ID}, registered, boom)
	assert.Equal(t, 200, sharedNameGET(t, byID, "/api/accounts/"+sharedNameSolo.ID).StatusCode)
}

// The other gate shapes take the same scope from getAccountScope: plan
// accounts (requirePlanAccess / requirePlanMutationAccess) and the
// resolveAccountNamesByID map (validatePurchaseRecommendationScope).
func TestAccountScope_SharedDisplayName_OtherGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	session := &Session{UserID: "u-1"}
	registered := []config.CloudAccount{sharedNameAWS, sharedNameAzure, sharedNameSolo}

	for _, tc := range []struct {
		name  string
		scope []string
		allow bool
	}{
		{"shared name", []string{"prod"}, false},
		{"ID entry", []string{sharedNameAWS.ID}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := sharedNameHandler(t, tc.scope, registered, nil)
			store := h.config.(*MockConfigStore)
			store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
				return []config.CloudAccount{sharedNameAWS}, nil
			}
			check := func(err error) {
				if tc.allow {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
				}
			}
			check(h.requirePlanAccess(ctx, session, "plan-1"))
			check(h.requirePlanMutationAccess(ctx, session, "plan-1"))
			id := sharedNameAWS.ID
			check(h.validatePurchaseRecommendationScope(ctx, session, []config.RecommendationRecord{{CloudAccountID: &id}}))
		})
	}
}
