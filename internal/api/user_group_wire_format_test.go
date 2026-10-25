package api

// Issue #169 pins the user and group write endpoints' wire format across the
// AuthServiceInterface refactor that replaced the any request and response
// types with the concrete auth.API* types. Each case drives the full
// HandleRequest path (JSON decode, router, handler, interface call, JSON
// encode), asserts the exact request value that crosses the interface, and
// compares the response body byte for byte against a literal recorded on the
// pre-refactor code. The test source is identical before and after the
// refactor, so a pass on both sides shows the wire format did not move.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	wireActorID   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	wireUserID    = "11111111-1111-4111-8111-111111111111"
	wireGroupID   = "22222222-2222-4222-8222-222222222222"
	wireGroupIDB  = "33333333-3333-4333-8333-333333333333"
	wireAccountID = "2f1c8e40-0000-4000-8000-000000000169"
	wireToken     = "session-169"
	wireCSRF      = "csrf-169"
	wirePassword  = "Wire-169-pass"
)

type wireCase struct {
	name     string
	method   string
	path     string
	body     string
	mockName string
	// args are the expected leading interface arguments after ctx and
	// before the request value.
	args     []any
	wantReq  any
	response any
	wantBody string
}

// wireDecodedConstraint decodes a constraint fragment the way the handler
// does, so the expected request carries the unexported explicit-max_amount
// marker that only UnmarshalJSON sets (issue #133). Comparing against it
// pins that the marker survives the interface call.
func wireDecodedConstraint(t *testing.T, fragment string) *auth.APIPermissionConstraint {
	t.Helper()
	c := new(auth.APIPermissionConstraint)
	require.NoError(t, json.Unmarshal([]byte(fragment), c))
	return c
}

const wireConstraintJSON = `{"accounts":["` + wireAccountID + `"],"providers":["aws"],"services":["ec2"],"regions":["us-east-1"],"max_amount":250.5}`

func wireFormatCases(t *testing.T) []wireCase {
	t.Helper()
	inviteSent := false
	inactive := false
	return []wireCase{
		{
			name:     "POST /api/users",
			method:   "POST",
			path:     "/api/users",
			body:     `{"email":"new@example.com","password":"` + base64.StdEncoding.EncodeToString([]byte(wirePassword)) + `","groups":["` + wireGroupID + `"]}`,
			mockName: "CreateUserAPI",
			args:     []any{wireActorID},
			wantReq: auth.APICreateUserRequest{
				Email:    "new@example.com",
				Password: wirePassword,
				Groups:   []string{wireGroupID},
			},
			response: &auth.APICreateUserResponse{
				APIUser: &auth.APIUser{
					ID:        wireUserID,
					Email:     "new@example.com",
					CreatedAt: "2026-01-02T03:04:05Z",
					UpdatedAt: "2026-01-02T03:04:05Z",
					Groups:    []string{wireGroupID},
					Active:    true,
				},
				InviteEmailSent:  &inviteSent,
				InviteEmailError: "smtp down",
			},
			wantBody: `{"id":"11111111-1111-4111-8111-111111111111","email":"new@example.com","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z","groups":["22222222-2222-4222-8222-222222222222"],"mfa_enabled":false,"active":true,"invite_email_sent":false,"invite_email_error":"smtp down"}`,
		},
		{
			name:     "PUT /api/users/{id}",
			method:   "PUT",
			path:     "/api/users/" + wireUserID,
			body:     `{"email":"renamed@example.com","groups":["` + wireGroupID + `","` + wireGroupIDB + `"],"active":false}`,
			mockName: "UpdateUserAPI",
			args:     []any{wireActorID, wireUserID},
			wantReq: auth.APIUpdateUserRequest{
				Email:  "renamed@example.com",
				Groups: []string{wireGroupID, wireGroupIDB},
				Active: &inactive,
			},
			response: &auth.APIUser{
				ID:         wireUserID,
				Email:      "renamed@example.com",
				CreatedAt:  "2026-01-02T03:04:05Z",
				UpdatedAt:  "2026-02-03T04:05:06Z",
				LastLogin:  "2026-02-01T00:00:00Z",
				Groups:     []string{wireGroupID, wireGroupIDB},
				MFAEnabled: true,
				Active:     false,
			},
			wantBody: `{"id":"11111111-1111-4111-8111-111111111111","email":"renamed@example.com","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-02-03T04:05:06Z","last_login":"2026-02-01T00:00:00Z","groups":["22222222-2222-4222-8222-222222222222","33333333-3333-4333-8333-333333333333"],"mfa_enabled":true,"active":false}`,
		},
		{
			name:     "POST /api/groups",
			method:   "POST",
			path:     "/api/groups",
			body:     `{"name":"Ops","description":"Operators","permissions":[{"action":"view","resource":"plans","constraints":` + wireConstraintJSON + `}],"allowed_accounts":["` + wireAccountID + `"]}`,
			mockName: "CreateGroupAPI",
			args:     []any{wireActorID},
			wantReq: auth.APICreateGroupRequest{
				Name:        "Ops",
				Description: "Operators",
				Permissions: []auth.APIPermission{{
					Action:      "view",
					Resource:    "plans",
					Constraints: wireDecodedConstraint(t, wireConstraintJSON),
				}},
				AllowedAccounts: []string{wireAccountID},
			},
			response: &auth.APIGroup{
				ID:          wireGroupID,
				Name:        "Ops",
				Description: "Operators",
				CreatedAt:   "2026-01-02T03:04:05Z",
				UpdatedAt:   "2026-01-02T03:04:05Z",
				Permissions: []auth.APIPermission{{
					Action:   "view",
					Resource: "plans",
					Constraints: &auth.APIPermissionConstraint{
						Accounts:  []string{wireAccountID},
						Providers: []string{"aws"},
						Services:  []string{"ec2"},
						Regions:   []string{"us-east-1"},
						MaxAmount: 250.5,
					},
				}},
				AllowedAccounts: []string{wireAccountID},
			},
			wantBody: `{"id":"22222222-2222-4222-8222-222222222222","name":"Ops","description":"Operators","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z","permissions":[{"constraints":{"accounts":["2f1c8e40-0000-4000-8000-000000000169"],"providers":["aws"],"services":["ec2"],"regions":["us-east-1"],"max_amount":250.5},"action":"view","resource":"plans"}],"allowed_accounts":["2f1c8e40-0000-4000-8000-000000000169"]}`,
		},
		{
			// An explicit empty allowed_accounts must reach the service as a
			// non-nil empty slice (it clears the restriction), and omitted
			// permissions as nil ("not sent").
			name:     "PUT /api/groups/{id}",
			method:   "PUT",
			path:     "/api/groups/" + wireGroupID,
			body:     `{"name":"Ops 2","allowed_accounts":[]}`,
			mockName: "UpdateGroupAPI",
			args:     []any{wireActorID, wireGroupID},
			wantReq: auth.APIUpdateGroupRequest{
				Name:            "Ops 2",
				AllowedAccounts: []string{},
			},
			response: &auth.APIGroup{
				ID:              wireGroupID,
				Name:            "Ops 2",
				CreatedAt:       "2026-01-02T03:04:05Z",
				UpdatedAt:       "2026-02-03T04:05:06Z",
				Permissions:     []auth.APIPermission{{Action: "view", Resource: "plans"}},
				AllowedAccounts: []string{},
			},
			wantBody: `{"id":"22222222-2222-4222-8222-222222222222","name":"Ops 2","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-02-03T04:05:06Z","permissions":[{"action":"view","resource":"plans"}],"allowed_accounts":[]}`,
		},
	}
}

func TestUserGroupWrites_WireFormat(t *testing.T) {
	for _, tc := range wireFormatCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			mockAuth := new(MockAuthService)
			mockAuth.On("ValidateSession", mock.Anything, wireToken).
				Return(&Session{UserID: wireActorID, Email: "admin@example.com"}, nil)
			mockAuth.On("ValidateCSRFToken", mock.Anything, wireToken, wireCSRF).Return(nil)
			mockAuth.grantAdmin()

			var gotReq any
			callArgs := append([]any{mock.Anything}, tc.args...)
			callArgs = append(callArgs, mock.Anything)
			reqIdx := len(callArgs) - 1
			mockAuth.On(tc.mockName, callArgs...).
				Run(func(args mock.Arguments) { gotReq = args.Get(reqIdx) }).
				Return(tc.response, nil).Once()

			h := &Handler{auth: mockAuth}
			resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
				Headers: map[string]string{
					"Authorization": "Bearer " + wireToken,
					"Content-Type":  "application/json",
					"X-CSRF-Token":  wireCSRF,
				},
				Body: tc.body,
				RequestContext: events.LambdaFunctionURLRequestContext{
					HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: tc.method, Path: tc.path},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, resp)

			require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
			assert.Equal(t, tc.wantReq, gotReq, "request value crossing AuthServiceInterface")
			assert.Equal(t, tc.wantBody, resp.Body, "response body bytes")
			mockAuth.AssertExpectations(t)
		})
	}
}

// TestAuthServiceInterface_UserGroupWritesAreTyped pins the four user and
// group write methods to their concrete request and response types, so a
// wrong request type is a compile error instead of the runtime "invalid
// request type" the any signatures allowed (issue #169).
func TestAuthServiceInterface_UserGroupWritesAreTyped(t *testing.T) {
	iface := reflect.TypeOf((*AuthServiceInterface)(nil)).Elem()
	cases := []struct {
		method string
		req    reflect.Type
		resp   reflect.Type
	}{
		{"CreateUserAPI", reflect.TypeOf(auth.APICreateUserRequest{}), reflect.TypeOf(&auth.APICreateUserResponse{})},
		{"UpdateUserAPI", reflect.TypeOf(auth.APIUpdateUserRequest{}), reflect.TypeOf(&auth.APIUser{})},
		{"CreateGroupAPI", reflect.TypeOf(auth.APICreateGroupRequest{}), reflect.TypeOf(&auth.APIGroup{})},
		{"UpdateGroupAPI", reflect.TypeOf(auth.APIUpdateGroupRequest{}), reflect.TypeOf(&auth.APIGroup{})},
	}
	for _, tc := range cases {
		m, ok := iface.MethodByName(tc.method)
		require.True(t, ok, tc.method)
		in := m.Type.In(m.Type.NumIn() - 1)
		assert.Equal(t, tc.req.String(), in.String(), "%s request parameter type", tc.method)
		require.Equal(t, 2, m.Type.NumOut(), tc.method)
		assert.Equal(t, tc.resp.String(), m.Type.Out(0).String(), "%s response type", tc.method)
	}
}
