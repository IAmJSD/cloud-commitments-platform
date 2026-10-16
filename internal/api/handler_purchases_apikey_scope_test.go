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

// Issue #979: the planned-purchase management routes granted full scope on
// update-any (and DELETE also on cancel-any) by asking HasPermissionAPI about
// the key's OWNER. A user API key holding only a narrow verb could therefore
// act on any user's scheduled purchase whenever its owner held update-any.
// These tests drive HandleRequest (router, middleware, handler) into a real
// auth.Service backed by auth.MockStore, so the key-versus-owner decision is
// the production one: key permissions intersected with the owner's.

const (
	keyScopeOwnerID = "aaaa9790-0000-4000-8000-000000000979"
	keyScopeVictim  = "bbbb9790-0000-4000-8000-000000000979"
	keyScopeExecID  = "cccc9790-0000-4000-8000-000000000979"
	keyScopeKeyID   = "key-979"
	keyScopeRawKey  = "raw-user-key-979"
	keyScopeToken   = "owner-session-979"
	keyScopeGroupID = "group-979"
)

var errKeyScopeReached = errors.New("mutation reached")

// keyScopeAuth sends every permission decision and API-key validation to a
// real auth.Service; session validation, CSRF and account scope stay mocked.
type keyScopeAuth struct {
	*MockAuthService
	service *auth.Service
}

func (a *keyScopeAuth) ValidateUserAPIKeyAPI(ctx context.Context, apiKey string) (any, any, error) {
	key, user, err := a.service.ValidateUserAPIKeyAPI(ctx, apiKey)
	if err != nil {
		return nil, nil, err
	}
	return key, user, nil
}

func (a *keyScopeAuth) HasAPIKeyPermissionAPI(ctx context.Context, apiKey, action, resource string) (userID, keyID string, allowed bool, err error) {
	return a.service.HasAPIKeyPermissionAPI(ctx, apiKey, action, resource)
}

func (a *keyScopeAuth) HasAPIKeyPermissionForConstraintsAPI(ctx context.Context, keyID, userID, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasAPIKeyPermissionForConstraintsAPI(ctx, keyID, userID, action, resource, sets)
}

func (a *keyScopeAuth) HasPermissionAPI(ctx context.Context, userID, action, resource string) (bool, error) {
	return a.service.HasPermissionAPI(ctx, userID, action, resource)
}

func (a *keyScopeAuth) HasPermissionForConstraintsAPI(ctx context.Context, userID, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasPermissionForConstraintsAPI(ctx, userID, action, resource, sets)
}

func purchasesPerms(verbs ...string) []auth.Permission {
	perms := make([]auth.Permission, 0, len(verbs))
	for _, v := range verbs {
		perms = append(perms, auth.Permission{Action: v, Resource: auth.ResourcePurchases})
	}
	return perms
}

// keyScopeOwnerPerms is the owner's group: every verb the four routes accept,
// including the full-scope update-any and cancel-any.
var keyScopeOwnerPerms = purchasesPerms(auth.ActionUpdateAny, auth.ActionCancelAny, auth.ActionCancelOwn,
	auth.ActionDelete, auth.ActionUpdate, auth.ActionExecute)

func newKeyScopeAuth(keyPerms []auth.Permission) *keyScopeAuth {
	sum := sha256.Sum256([]byte(keyScopeRawKey))
	expires := time.Now().Add(24 * time.Hour)
	key := &auth.UserAPIKey{
		ID: keyScopeKeyID, UserID: keyScopeOwnerID, KeyHash: base64.RawURLEncoding.EncodeToString(sum[:]),
		IsActive: true, ExpiresAt: &expires, Permissions: keyPerms,
	}
	store := new(auth.MockStore)
	store.On("GetAPIKeyByHash", mock.Anything, key.KeyHash).Return(key, nil).Maybe()
	store.On("GetAPIKeyByID", mock.Anything, keyScopeKeyID).Return(key, nil).Maybe()
	store.On("GetUserByID", mock.Anything, keyScopeOwnerID).Return(&auth.User{
		ID: keyScopeOwnerID, Email: "owner@example.com", Active: true, GroupIDs: []string{keyScopeGroupID},
	}, nil).Maybe()
	store.On("GetGroup", mock.Anything, keyScopeGroupID).Return(&auth.Group{
		ID: keyScopeGroupID, Permissions: keyScopeOwnerPerms,
	}, nil).Maybe()

	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, keyScopeToken).Return(&Session{UserID: keyScopeOwnerID, Email: "owner@example.com"}, nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("GetAllowedAccountsAPI", mock.Anything, keyScopeOwnerID).Return([]string{}, nil).Maybe()
	return &keyScopeAuth{MockAuthService: m, service: auth.NewService(auth.ServiceConfig{Store: store, CSRFKey: auth.TestCSRFKey()})}
}

// keyScopeExec is a not-yet-due row carrying one priced recommendation, so
// run's execute Constraints check can evaluate it.
func keyScopeExec(status, creator string) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID: keyScopeExecID, Status: status, CreatedByUserID: &creator,
		ScheduledDate: time.Now().Add(72 * time.Hour),
		Recommendations: []config.RecommendationRecord{{
			ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1", Count: 1, UpfrontCost: 100,
		}},
	}
}

type keyScopeRoute struct {
	name, method, suffix, status string
	// narrow is each key scope that passes the route's entry gate but holds
	// no full-scope verb.
	narrow [][]string
}

var keyScopeRoutes = []keyScopeRoute{
	{name: "delete", method: http.MethodDelete, status: "pending",
		narrow: [][]string{{auth.ActionDelete}, {auth.ActionCancelOwn}}},
	{name: "pause", method: http.MethodPost, suffix: "/pause", status: "pending",
		narrow: [][]string{{auth.ActionUpdate}}},
	{name: "resume", method: http.MethodPost, suffix: "/resume", status: "paused",
		narrow: [][]string{{auth.ActionUpdate}}},
	{name: "run", method: http.MethodPost, suffix: "/run", status: "pending",
		narrow: [][]string{{auth.ActionExecute}}},
}

func keyScopeRequest(rt keyScopeRoute, headers map[string]string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: headers,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: rt.method, Path: "/api/purchases/planned/" + keyScopeExecID + rt.suffix,
			},
		},
	}
}

func keyHeaders() map[string]string { return map[string]string{"X-API-Key": keyScopeRawKey} }

func ownerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + keyScopeToken, "X-CSRF-Token": "csrf-979"}
}

// serveKeyScope sends the request and reports the status and whether the
// route reached its state-changing call. Every mutation answers
// errKeyScopeReached, so an allowed request ends in a 500 after the call.
func serveKeyScope(t *testing.T, rt keyScopeRoute, a *keyScopeAuth, creator string, headers map[string]string) (int, bool) {
	t.Helper()
	store := new(MockConfigStore)
	purchaseMgr := new(MockPurchaseManager)
	store.On("GetExecutionByID", mock.Anything, keyScopeExecID).Return(keyScopeExec(rt.status, creator), nil)
	store.On("TransitionExecutionStatus", mock.Anything, keyScopeExecID, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errKeyScopeReached).Maybe()
	purchaseMgr.On("RunPlannedPurchaseNow", mock.Anything, keyScopeExecID, mock.Anything, mock.Anything).
		Return(nil, "", errKeyScopeReached).Maybe()

	h := &Handler{auth: a, config: store, purchase: purchaseMgr}
	resp, err := h.HandleRequest(context.Background(), keyScopeRequest(rt, headers))
	require.NoError(t, err)
	require.NotNil(t, resp)

	reached := false
	for _, c := range store.Calls {
		reached = reached || c.Method == "TransitionExecutionStatus"
	}
	for _, c := range purchaseMgr.Calls {
		reached = reached || c.Method == "RunPlannedPurchaseNow"
	}
	if !reached {
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		purchaseMgr.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	}
	return resp.StatusCode, reached
}

// TestPlannedPurchaseRoutes_FullScopeFollowsTheAPIKey is the #979 regression:
// a key without update-any or cancel-any must not act on another user's
// scheduled purchase through its owner's grant.
func TestPlannedPurchaseRoutes_FullScopeFollowsTheAPIKey(t *testing.T) {
	for _, rt := range keyScopeRoutes {
		for _, verbs := range rt.narrow {
			t.Run(rt.name+"/key "+verbs[0]+" on another user's row is refused", func(t *testing.T) {
				code, reached := serveKeyScope(t, rt, newKeyScopeAuth(purchasesPerms(verbs...)), keyScopeVictim, keyHeaders())
				assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, code)
				assert.False(t, reached, "a refused request must not reach the mutation")
			})
			t.Run(rt.name+"/key "+verbs[0]+" on its owner's row is allowed", func(t *testing.T) {
				_, reached := serveKeyScope(t, rt, newKeyScopeAuth(purchasesPerms(verbs...)), keyScopeOwnerID, keyHeaders())
				assert.True(t, reached, "the creator control must reach the mutation")
			})
			t.Run(rt.name+"/key "+verbs[0]+" plus update-any on another user's row is allowed", func(t *testing.T) {
				perms := purchasesPerms(append([]string{auth.ActionUpdateAny}, verbs...)...)
				_, reached := serveKeyScope(t, rt, newKeyScopeAuth(perms), keyScopeVictim, keyHeaders())
				assert.True(t, reached, "a key that holds update-any keeps full scope")
			})
		}
		t.Run(rt.name+"/owner session on another user's row is allowed", func(t *testing.T) {
			_, reached := serveKeyScope(t, rt, newKeyScopeAuth(nil), keyScopeVictim, ownerHeaders())
			assert.True(t, reached, "the owner's own session keeps update-any")
		})
	}
}

// TestDeletePlannedPurchase_KeyCancelAnyKeepsFullScope: on DELETE, cancel-any
// held by the key (not only by its owner) still cancels another user's row.
func TestDeletePlannedPurchase_KeyCancelAnyKeepsFullScope(t *testing.T) {
	rt := keyScopeRoutes[0]
	_, reached := serveKeyScope(t, rt, newKeyScopeAuth(purchasesPerms(auth.ActionCancelAny)), keyScopeVictim, keyHeaders())
	assert.True(t, reached)
}

// TestPlannedPurchaseRoutes_APIKeyFullScopeProbeErrorRefuses: an error
// probing the key's update-any refuses the request. It must not be read as
// "not held" and fall through to the creator branch, which would allow the
// key owner's own row.
func TestPlannedPurchaseRoutes_APIKeyFullScopeProbeErrorRefuses(t *testing.T) {
	cases := []struct {
		name, entryVerb string
		call            func(h *Handler, req *events.LambdaFunctionURLRequest) error
	}{
		{"pause", auth.ActionUpdate, func(h *Handler, req *events.LambdaFunctionURLRequest) error {
			_, err := h.pausePlannedPurchase(context.Background(), req, keyScopeExecID)
			return err
		}},
		{"delete", auth.ActionDelete, func(h *Handler, req *events.LambdaFunctionURLRequest) error {
			_, err := h.deletePlannedPurchase(context.Background(), req, keyScopeExecID)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boom := errors.New("key store down")
			store := new(MockConfigStore)
			store.On("GetExecutionByID", mock.Anything, keyScopeExecID).Return(keyScopeExec("pending", keyScopeOwnerID), nil).Maybe()
			store.On("TransitionExecutionStatus", mock.Anything, keyScopeExecID, mock.Anything, mock.Anything, mock.Anything).
				Return(nil, errKeyScopeReached).Maybe()
			m := new(MockAuthService)
			m.On("HasAPIKeyPermissionAPI", mock.Anything, keyScopeRawKey, tc.entryVerb, auth.ResourcePurchases).
				Return(keyScopeOwnerID, keyScopeKeyID, true, nil)
			m.On("GetAllowedAccountsAPI", mock.Anything, keyScopeOwnerID).Return([]string{}, nil)
			probe := func(verb string) *mock.Call {
				return m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, keyScopeKeyID, keyScopeOwnerID, verb,
					auth.ResourcePurchases, []auth.PermissionConstraints{{}})
			}
			probe(auth.ActionUpdateAny).Return(false, boom)
			// Reached only if the error were swallowed; the fallback would then allow.
			probe(auth.ActionCancelAny).Return(false, nil).Maybe()
			probe(auth.ActionCancelOwn).Return(false, nil).Maybe()

			h := &Handler{auth: m, config: store}
			err := tc.call(h, &events.LambdaFunctionURLRequest{Headers: keyHeaders()})
			require.ErrorIs(t, err, boom)
			store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}
