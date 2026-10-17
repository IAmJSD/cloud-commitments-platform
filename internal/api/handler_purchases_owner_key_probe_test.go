package api //nolint:revive // var-naming: package name "api" is intentional for handler package

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1005: the purchase authorization helpers asked HasPermissionAPI
// whether the key's OWNER held the full-scope *-any verb, while the
// Constraints that follow are judged on the key itself. These tests drive
// the decision into a real auth.Service backed by auth.MockStore (key
// permissions intersected with the owner's), the same shape #1001 uses for
// the planned-purchase routes.

const (
	okpOwnerID  = "aaaa1005-0000-4000-8000-00000000a005"
	okpVictimID = "bbbb1005-0000-4000-8000-00000000b005"
	okpRowID    = "cccc1005-0000-4000-8000-00000000c005"
	okpKeyID    = "key-1005"
	okpRawKey   = "raw-user-key-1005"
	okpToken    = "owner-session-1005"
	okpGroupID  = "group-1005"
)

var errOKPReached = errors.New("action reached")

// okpAuth sends every permission decision and API-key validation to a real
// auth.Service; session validation, CSRF and account scope stay mocked.
type okpAuth struct {
	*MockAuthService
	service *auth.Service
}

func (a *okpAuth) ValidateUserAPIKeyAPI(ctx context.Context, apiKey string) (any, any, error) {
	key, user, err := a.service.ValidateUserAPIKeyAPI(ctx, apiKey)
	if err != nil {
		return nil, nil, err
	}
	return key, user, nil
}

func (a *okpAuth) HasAPIKeyPermissionAPI(ctx context.Context, apiKey, action, resource string) (userID, keyID string, allowed bool, err error) {
	return a.service.HasAPIKeyPermissionAPI(ctx, apiKey, action, resource)
}

func (a *okpAuth) HasAPIKeyPermissionForConstraintsAPI(ctx context.Context, keyID, userID, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasAPIKeyPermissionForConstraintsAPI(ctx, keyID, userID, action, resource, sets)
}

func (a *okpAuth) HasPermissionAPI(ctx context.Context, userID, action, resource string) (bool, error) {
	return a.service.HasPermissionAPI(ctx, userID, action, resource)
}

func (a *okpAuth) HasPermissionForConstraintsAPI(ctx context.Context, userID, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasPermissionForConstraintsAPI(ctx, userID, action, resource, sets)
}

func okpPerms(verbs ...string) []auth.Permission {
	perms := make([]auth.Permission, 0, len(verbs))
	for _, v := range verbs {
		perms = append(perms, auth.Permission{Action: v, Resource: auth.ResourcePurchases})
	}
	return perms
}

// okpOwnerVerbs is the owner's group: every verb the helpers probe,
// including each full-scope *-any.
var okpOwnerVerbs = []string{
	auth.ActionExecute, auth.ActionExecuteAny, auth.ActionExecuteOwn,
	auth.ActionApproveAny, auth.ActionApproveOwn,
	auth.ActionCancelAny, auth.ActionCancelOwn,
	auth.ActionRetryAny, auth.ActionRetryOwn,
	auth.ActionRevokeAny, auth.ActionRevokeOwn,
}

// newOKPAuth builds the owner (holding okpOwnerVerbs) and a user API key
// scoped to keyVerbs.
func newOKPAuth(keyVerbs ...string) *okpAuth {
	sum := sha256.Sum256([]byte(okpRawKey))
	expires := time.Now().Add(24 * time.Hour)
	key := &auth.UserAPIKey{
		ID: okpKeyID, UserID: okpOwnerID, KeyHash: base64.RawURLEncoding.EncodeToString(sum[:]),
		IsActive: true, ExpiresAt: &expires, Permissions: okpPerms(keyVerbs...),
	}
	store := new(auth.MockStore)
	store.On("GetAPIKeyByHash", mock.Anything, key.KeyHash).Return(key, nil).Maybe()
	store.On("GetAPIKeyByID", mock.Anything, okpKeyID).Return(key, nil).Maybe()
	store.On("GetUserByID", mock.Anything, okpOwnerID).Return(&auth.User{
		ID: okpOwnerID, Email: "owner@example.com", Active: true, GroupIDs: []string{okpGroupID},
	}, nil).Maybe()
	store.On("GetGroup", mock.Anything, okpGroupID).Return(&auth.Group{
		ID: okpGroupID, Permissions: okpPerms(okpOwnerVerbs...),
	}, nil).Maybe()

	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, okpToken).Return(&Session{UserID: okpOwnerID, Email: "owner@example.com"}, nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("GetAllowedAccountsAPI", mock.Anything, okpOwnerID).Return([]string{}, nil).Maybe()
	return &okpAuth{MockAuthService: m, service: auth.NewService(auth.ServiceConfig{Store: store, CSRFKey: auth.TestCSRFKey()})}
}

func okpKeyHeaders() map[string]string { return map[string]string{"X-API-Key": okpRawKey} }

func okpOwnerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + okpToken, "X-CSRF-Token": "csrf-1005"}
}

func okpRequest(method, path, body string, headers map[string]string) *events.LambdaFunctionURLRequest {
	headers["Content-Type"] = "application/json"
	return &events.LambdaFunctionURLRequest{
		Headers: headers,
		Body:    body,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: path},
		},
	}
}

// serveOKPExecute posts a direct execute through HandleRequest and reports
// the status and whether the purchase reached DirectExecute (which answers
// errOKPReached, so an allowed request ends in an error after the call).
func serveOKPExecute(t *testing.T, a *okpAuth, headers map[string]string) (int, bool) {
	t.Helper()
	store := new(MockConfigStore)
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil).Maybe()
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("GetPendingExecutions", mock.Anything).Return([]config.PurchaseExecution{}, nil).Maybe()
	expectStoredRecs(store, config.RecommendationRecord{
		ID: "rec-1", Provider: "aws", Service: "ec2", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 500, Savings: 100,
	})
	purchaseMgr := new(MockPurchaseManager)
	purchaseMgr.On("DirectExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, "", errOKPReached).Maybe()

	h := &Handler{auth: a, config: store, purchase: purchaseMgr}
	resp, err := h.HandleRequest(context.Background(), okpRequest(http.MethodPost, "/api/purchases/execute", directExecRecBody, headers))
	require.NoError(t, err)
	require.NotNil(t, resp)

	reached := false
	for _, c := range purchaseMgr.Calls {
		reached = reached || c.Method == "DirectExecute"
	}
	if !reached {
		purchaseMgr.AssertNotCalled(t, "DirectExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	}
	return resp.StatusCode, reached
}

// TestExecuteDirect_FullScopeFollowsTheAPIKey: direct execute is the one
// site an API-key session reaches (POST /api/purchases/execute accepts
// X-API-Key). The row is always created by the caller, so there is no other
// user's row to reach; the owner-probe bug made a key holding execute-own
// take the execute-any branch whenever its owner held execute-any, and then
// fail the key's execute-any Constraints check.
func TestExecuteDirect_FullScopeFollowsTheAPIKey(t *testing.T) {
	t.Run("key without execute-any or execute-own is refused", func(t *testing.T) {
		code, reached := serveOKPExecute(t, newOKPAuth(auth.ActionExecute), okpKeyHeaders())
		assert.Equal(t, http.StatusForbidden, code)
		assert.False(t, reached, "a refused direct execute must not reach the purchase")
	})
	t.Run("key with execute-own executes its own submission", func(t *testing.T) {
		_, reached := serveOKPExecute(t, newOKPAuth(auth.ActionExecute, auth.ActionExecuteOwn), okpKeyHeaders())
		assert.True(t, reached, "execute-own on the key must not be judged by the owner's execute-any")
	})
	t.Run("key with execute-any executes", func(t *testing.T) {
		_, reached := serveOKPExecute(t, newOKPAuth(auth.ActionExecute, auth.ActionExecuteAny), okpKeyHeaders())
		assert.True(t, reached)
	})
	t.Run("owner session executes", func(t *testing.T) {
		_, reached := serveOKPExecute(t, newOKPAuth(), okpOwnerHeaders())
		assert.True(t, reached)
	})
}

// TestExecuteDirect_APIKeyFullScopeProbeErrorRefuses: an error probing the
// key's execute-any refuses the request instead of being read as "not held".
func TestExecuteDirect_APIKeyFullScopeProbeErrorRefuses(t *testing.T) {
	boom := errors.New("key store down")
	store := new(MockConfigStore)
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
	store.On("GetPendingExecutions", mock.Anything).Return([]config.PurchaseExecution{}, nil).Maybe()
	expectStoredRecs(store, config.RecommendationRecord{
		ID: "rec-1", Provider: "aws", Service: "ec2", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 500, Savings: 100,
	})
	purchaseMgr := new(MockPurchaseManager)
	purchaseMgr.On("DirectExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, "", errOKPReached).Maybe()

	m := new(MockAuthService)
	m.On("HasAPIKeyPermissionAPI", mock.Anything, okpRawKey, auth.ActionExecute, auth.ResourcePurchases).
		Return(okpOwnerID, okpKeyID, true, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, okpOwnerID).Return([]string{}, nil)
	probe := []auth.PermissionConstraints{{}}
	m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, okpKeyID, okpOwnerID, auth.ActionExecuteAny, auth.ResourcePurchases, probe).
		Return(false, boom)
	// Reached only if the error were swallowed or the owner probed instead:
	// every other answer allows, so a regression would execute.
	m.On("HasPermissionAPI", mock.Anything, okpOwnerID, mock.Anything, auth.ResourcePurchases).Return(true, nil).Maybe()
	m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, okpKeyID, okpOwnerID, mock.Anything, auth.ResourcePurchases, mock.Anything).
		Return(true, nil).Maybe()

	h := &Handler{auth: m, config: store, purchase: purchaseMgr}
	_, err := h.executePurchase(context.Background(), &events.LambdaFunctionURLRequest{Headers: okpKeyHeaders(), Body: directExecRecBody})
	require.ErrorIs(t, err, boom)
	purchaseMgr.AssertNotCalled(t, "DirectExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestSessionOnlyPurchaseRoutes_RefuseAUserAPIKey documents why the other
// sites were not exploitable: approve, cancel, email revoke, retry, revoke
// and RI-exchange approve resolve their session through requireSession or
// tryGetSession, which only accept a bearer session (never a user-API-key
// session), so an X-API-Key request is refused before any helper runs,
// whatever the key or its owner holds. The helpers now probe the key anyway
// (see handler_purchases_owner_key_probe_helpers_test.go).
func TestSessionOnlyPurchaseRoutes_RefuseAUserAPIKey(t *testing.T) {
	routes := []struct {
		name, path, anyVerb, ownVerb string
	}{
		{"approve", "/api/purchases/approve/" + okpRowID, auth.ActionApproveAny, auth.ActionApproveOwn},
		{"cancel", "/api/purchases/cancel/" + okpRowID, auth.ActionCancelAny, auth.ActionCancelOwn},
		{"email revoke", "/api/purchases/revoke/" + okpRowID, auth.ActionCancelAny, auth.ActionCancelOwn},
		{"retry", "/api/purchases/retry/" + okpRowID, auth.ActionRetryAny, auth.ActionRetryOwn},
		{"revoke", "/api/purchases/" + okpRowID + "/revoke", auth.ActionRevokeAny, auth.ActionRevokeOwn},
		{"ri exchange approve", "/api/ri-exchange/approve/" + okpRowID, auth.ActionApproveAny, auth.ActionApproveOwn},
	}
	for _, rt := range routes {
		for _, verb := range []string{rt.ownVerb, rt.anyVerb} {
			t.Run(rt.name+"/key "+verb+" on another user's row", func(t *testing.T) {
				creator := okpVictimID
				store := new(MockConfigStore)
				store.On("GetExecutionByID", mock.Anything, okpRowID).Return(&config.PurchaseExecution{
					ExecutionID: okpRowID, Status: "failed", CreatedByUserID: &creator,
				}, nil).Maybe()
				store.On("GetRIExchangeRecord", mock.Anything, okpRowID).Return(&config.RIExchangeRecord{
					ID: okpRowID, Status: "pending", CreatedByUserID: &creator,
				}, nil).Maybe()
				purchaseMgr := new(MockPurchaseManager)

				h := &Handler{auth: newOKPAuth(verb), config: store, purchase: purchaseMgr}
				resp, err := h.HandleRequest(context.Background(), okpRequest(http.MethodPost, rt.path, "{}", okpKeyHeaders()))
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a user API key is not a session on this route")
				assert.Empty(t, purchaseMgr.Calls, "a refused request must not reach the purchase manager")
				for _, c := range store.Calls {
					assert.Contains(t, []string{"GetExecutionByID", "GetRIExchangeRecord"}, c.Method, "only lookups may run")
				}
			})
		}
	}
}
