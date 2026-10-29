package api //nolint:revive // var-naming: package name "api" is intentional for handler package

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #951: the create and list API-key responses must return permission
// constraints in the documented OpenAPI shape (constraints.accounts /
// constraints.max_amount), the same shape POST /api/api-keys accepts. These
// tests drive the real HTTP handler into the real auth.Service (backed by
// auth.MockStore, so no database). That the create path stores the
// constraints is covered at the service and adapter level by #957's tests.

const (
	roundtripUserID  = "user-951"
	roundtripToken   = "session-951"
	roundtripCSRF    = "csrf-951"
	roundtripAccount = "2f1c8e40-0000-4000-8000-000000000951"
	roundtripOwnerPw = "Owner-951-pass"
)

// realAPIKeyAuth answers session and permission checks from MockAuthService
// but sends the API-key create and list calls to a real auth.Service, so the
// handler-to-service conversion under test is the production one.
type realAPIKeyAuth struct {
	*MockAuthService
	service *auth.Service
}

func (a *realAPIKeyAuth) CreateAPIKeyAPI(ctx context.Context, userID string, req auth.APICreateAPIKeyRequest) (any, error) {
	return a.service.CreateAPIKeyAPI(ctx, userID, req)
}

func (a *realAPIKeyAuth) ListUserAPIKeysAPI(ctx context.Context, userID string) (any, error) {
	return a.service.ListUserAPIKeysAPI(ctx, userID)
}

// newAPIKeyRoundtripHandler returns a handler wired to a real auth.Service,
// the store behind it, and a pointer that receives the key the service hands
// to the store.
func newAPIKeyRoundtripHandler(t *testing.T) (*Handler, *auth.MockStore, **auth.UserAPIKey) {
	t.Helper()
	store := new(auth.MockStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	user := &auth.User{
		ID:           roundtripUserID,
		Email:        "owner@example.com",
		Active:       true,
		PasswordHash: auth.TestPasswordHash(t, roundtripOwnerPw),
		GroupIDs:     []string{auth.DefaultAdminGroupID},
	}
	store.On("GetUserByID", mock.Anything, roundtripUserID).Return(user, nil)
	store.On("GetGroup", mock.Anything, auth.DefaultAdminGroupID).Return(&auth.Group{
		ID:          auth.DefaultAdminGroupID,
		Permissions: []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}},
	}, nil)

	stored := new(*auth.UserAPIKey)
	store.On("CreateAPIKey", mock.Anything, mock.AnythingOfType("*auth.UserAPIKey")).
		Run(func(args mock.Arguments) { *stored = args.Get(1).(*auth.UserAPIKey) }).
		Return(nil)

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, roundtripToken).
		Return(&Session{UserID: roundtripUserID, Email: user.Email}, nil)
	mockAuth.On("ValidateCSRFToken", mock.Anything, roundtripToken, roundtripCSRF).Return(nil).Maybe()
	mockAuth.grantAdmin()

	service := auth.NewService(auth.ServiceConfig{Store: store, CSRFKey: auth.TestCSRFKey()})
	return &Handler{auth: &realAPIKeyAuth{MockAuthService: mockAuth, service: service}}, store, stored
}

func apiKeyRoundtripRequest(t *testing.T, h *Handler, method, body string) *events.LambdaFunctionURLResponse {
	t.Helper()
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer " + roundtripToken,
			"Content-Type":  "application/json",
			"X-CSRF-Token":  roundtripCSRF,
		},
		Body: body,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: "/api/api-keys"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func constrainedAPIKeyBody(t *testing.T, maxAmount float64) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":       "scoped key",
		"password":   base64.StdEncoding.EncodeToString([]byte(roundtripOwnerPw)),
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"permissions": []map[string]any{{
			"action":   auth.ActionView,
			"resource": auth.ResourceRecommendations,
			"constraints": map[string]any{
				"accounts":   []string{roundtripAccount},
				"providers":  []string{"aws"},
				"services":   []string{"ec2"},
				"regions":    []string{"us-east-1"},
				"max_amount": maxAmount,
			},
		}},
	})
	require.NoError(t, err)
	return string(body)
}

// requireDocumentedConstraints asserts a serialized constraints object uses
// the OpenAPI field names and carries every dimension that was requested.
func requireDocumentedConstraints(t *testing.T, constraints map[string]any) {
	t.Helper()
	assert.Equal(t, []any{roundtripAccount}, constraints["accounts"])
	assert.Equal(t, 500.5, constraints["max_amount"])
	assert.Equal(t, []any{"aws"}, constraints["providers"])
	assert.Equal(t, []any{"ec2"}, constraints["services"])
	assert.Equal(t, []any{"us-east-1"}, constraints["regions"])
	assert.NotContains(t, constraints, "account_ids", "response must use the documented field name")
	assert.NotContains(t, constraints, "max_purchase_amount", "response must use the documented field name")
}

func firstPermissionConstraints(t *testing.T, perms any) map[string]any {
	t.Helper()
	list, ok := perms.([]any)
	require.True(t, ok, "permissions = %#v", perms)
	require.Len(t, list, 1)
	perm, ok := list[0].(map[string]any)
	require.True(t, ok)
	constraints, ok := perm["constraints"].(map[string]any)
	require.True(t, ok, "constraints missing from %#v", perm)
	return constraints
}

func TestHandleRequest_CreateAPIKey_ResponseUsesDocumentedConstraintShape(t *testing.T) {
	h, _, _ := newAPIKeyRoundtripHandler(t)

	resp := apiKeyRoundtripRequest(t, h, "POST", constrainedAPIKeyBody(t, 500.5))
	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)

	var created struct {
		Info struct {
			Permissions any `json:"permissions"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &created))
	requireDocumentedConstraints(t, firstPermissionConstraints(t, created.Info.Permissions))
}

func TestHandleRequest_ListAPIKeys_ReturnsDocumentedConstraintShape(t *testing.T) {
	h, store, _ := newAPIKeyRoundtripHandler(t)
	// Listing creates nothing and does not read the owner's group.
	relaxUnreachedStubs(&store.Mock, "GetGroup", "CreateAPIKey")

	// Seeded directly in the stored (auth) shape so this checks the listing
	// response independently of the create path.
	store.On("ListAPIKeysByUser", mock.Anything, roundtripUserID).Return([]*auth.UserAPIKey{{
		ID:       "key-951",
		UserID:   roundtripUserID,
		Name:     "scoped key",
		IsActive: true,
		Permissions: []auth.Permission{{
			Action:   auth.ActionView,
			Resource: auth.ResourceRecommendations,
			Constraints: &auth.PermissionConstraints{
				AccountIDs:        []string{roundtripAccount},
				Providers:         []string{"aws"},
				Services:          []string{"ec2"},
				Regions:           []string{"us-east-1"},
				MaxPurchaseAmount: 500.5,
			},
		}},
	}}, nil)

	resp := apiKeyRoundtripRequest(t, h, "GET", "")
	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)

	var listed struct {
		APIKeys []struct {
			Permissions any `json:"permissions"`
		} `json:"api_keys"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &listed))
	require.Len(t, listed.APIKeys, 1)
	requireDocumentedConstraints(t, firstPermissionConstraints(t, listed.APIKeys[0].Permissions))
}

// The negative spend-cap check from issue #83 runs on the converted
// permissions, so it must be reachable over HTTP, not only at the service
// level. Before #957 the negative max_amount was dropped during decoding and
// the key was minted with no cap at all.
func TestHandleRequest_CreateAPIKey_RejectsNegativeMaxAmount(t *testing.T) {
	h, store, stored := newAPIKeyRoundtripHandler(t)
	// The cap is refused before the owner's group is read or a key is
	// stored; *stored below pins the latter.
	relaxUnreachedStubs(&store.Mock, "GetGroup", "CreateAPIKey")

	resp := apiKeyRoundtripRequest(t, h, "POST", constrainedAPIKeyBody(t, -1))

	assert.Equal(t, 400, resp.StatusCode, "body: %s", resp.Body)
	assert.Contains(t, resp.Body, "invalid max purchase amount")
	// stored is set by the store's CreateAPIKey mock, so nil means the
	// service never wrote the key.
	assert.Nil(t, *stored, "a key with a negative cap must not be stored")
}
