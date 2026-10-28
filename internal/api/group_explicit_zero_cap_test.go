package api

// Issue #133: POST/PUT /api/groups reject an explicit max_amount of 0. The
// handler decodes the body itself, so this drives the real Handler into a real
// auth.Service (MockStore) to prove the explicit-zero signal survives the
// handler's JSON decode. The mock-auth handler tests cannot show that.

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockStore aliases auth.MockStore so the mock assertion guard, which only
// resolves receivers built from a bare type name, can check this file's
// AssertNotCalled sites.
type MockStore = auth.MockStore

const zeroCapGroupID = "33333333-3333-3333-3333-333333333333"

// realGroupAuth routes the group writes to a real auth.Service and leaves
// session and permission checks on the shared mock.
type realGroupAuth struct {
	*MockAuthService
	svc *auth.Service
}

func (a *realGroupAuth) CreateGroupAPI(ctx context.Context, actor string, req auth.APICreateGroupRequest) (*auth.APIGroup, error) {
	return a.svc.CreateGroupAPI(ctx, actor, req)
}

func (a *realGroupAuth) UpdateGroupAPI(ctx context.Context, actor, id string, req auth.APIUpdateGroupRequest) (*auth.APIGroup, error) {
	return a.svc.UpdateGroupAPI(ctx, actor, id, req)
}

func TestGroupWritesHTTP_ExplicitZeroSpendCap(t *testing.T) {
	ctx := context.Background()
	body := func(constraints string) string {
		return `{"name":"Team","permissions":[{"action":"view","resource":"plans","constraints":` + constraints + `}]}`
	}
	cases := []struct {
		name        string
		constraints string
		wantStatus  int
	}{
		{"explicit zero", `{"max_amount":0}`, 400},
		{"explicit 0.0", `{"max_amount":0.0}`, 400},
		{"absent", `{"regions":["us-east-1"]}`, 0},
		{"positive", `{"max_amount":5000}`, 0},
	}
	for _, tc := range cases {
		run := func(t *testing.T, write func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error)) {
			store := new(MockStore)
			store.On("GetGroup", mock.Anything, zeroCapGroupID).Return(&auth.Group{ID: zeroCapGroupID, Name: "Team"}, nil).Maybe()
			store.On("CreateGroup", mock.Anything, mock.Anything).Return(nil).Maybe()
			store.On("UpdateGroup", mock.Anything, mock.Anything).Return(nil).Maybe()
			// The PUT gives the empty group its first permission, so the
			// issue #1409 member check lists users; the group has none.
			store.On("ListUsers", mock.Anything).Return([]auth.User{}, nil).Maybe()
			mockAuth := new(MockAuthService)
			mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: auth.AdminAPIKeyActorID}, nil)
			mockAuth.grantAdmin()
			h := &Handler{auth: &realGroupAuth{MockAuthService: mockAuth, svc: auth.NewService(auth.ServiceConfig{Store: store})}}
			req := &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"Authorization": "Bearer admin-token"},
				Body:    body(tc.constraints),
			}

			_, err := write(h, req)

			if tc.wantStatus == 0 {
				require.NoError(t, err)
				return
			}
			var ce *clientError
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.wantStatus, ce.code)
			assert.Contains(t, ce.message, "max_amount must be greater than 0")
			store.AssertNotCalled(t, "CreateGroup", mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
		}
		t.Run("POST "+tc.name, func(t *testing.T) {
			run(t, func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) { return h.createGroup(ctx, req) })
		})
		t.Run("PUT "+tc.name, func(t *testing.T) {
			run(t, func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
				return h.updateGroup(ctx, req, zeroCapGroupID)
			})
		})
	}
}
