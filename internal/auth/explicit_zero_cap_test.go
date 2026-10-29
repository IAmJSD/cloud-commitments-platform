package auth

// An explicit max_amount of 0 on a group write is rejected (issue #133). Absent
// or null still means "no cap". Only wire JSON can say "explicit", so every
// case here decodes a JSON body the way the HTTP handlers do.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func groupBodyWithCap(t *testing.T, constraints string) APICreateGroupRequest {
	t.Helper()
	var req APICreateGroupRequest
	body := `{"name":"Team","permissions":[{"action":"view","resource":"plans","constraints":` + constraints + `}]}`
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	return req
}

func TestGroupWrite_RejectsExplicitZeroMaxAmount(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{`0`, `0.0`, `-0`, `0e5`} {
		constraints := `{"max_amount":` + raw + `}`

		t.Run("create "+raw, func(t *testing.T) {
			mockStore := new(MockStore)
			svc := newCeilingService(t, mockStore)
			stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
			mockStore.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()

			_, err := svc.CreateGroupAPI(ctx, ceilingActorID, groupBodyWithCap(t, constraints))

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPermission)
			assert.Contains(t, err.Error(), "max_amount must be greater than 0")
			assert.Contains(t, err.Error(), "entry 0")
			mockStore.AssertNotCalled(t, "CreateGroup", mock.Anything, mock.Anything)
		})

		t.Run("update "+raw, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			svc := newCeilingService(t, mockStore)
			stubTargetGroup(ctx, mockStore, &Group{ID: ceilingTargetID, Name: "Team"})
			stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
			mockStore.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()
			var req APIUpdateGroupRequest
			require.NoError(t, json.Unmarshal([]byte(
				`{"permissions":[{"action":"view","resource":"plans","constraints":`+constraints+`}]}`), &req))

			_, err := svc.UpdateGroupAPI(ctx, ceilingActorID, ceilingTargetID, req)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPermission)
			mockStore.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
		})
	}
}

// The shared adapter also guards CreateAPIKeyAPI. The HTTP key handler decodes
// auth.APICreateAPIKeyRequest itself, so this is the path it takes (#980).
func TestCreateAPIKeyAPI_RejectsDecodedExplicitZeroMaxAmount(t *testing.T) {
	var req APICreateAPIKeyRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"name":"k","permissions":[{"action":"view","resource":"plans","constraints":{"max_amount":0}}]}`), &req))
	svc := newCeilingService(t, new(MockStore))

	_, err := svc.CreateAPIKeyAPI(context.Background(), "user-1", req)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPermission)
}

func TestGroupWrite_AbsentNullAndPositiveMaxAmountAreNotRejected(t *testing.T) {
	ctx := context.Background()
	cases := map[string]float64{
		`{}`:                    0,
		`{"max_amount":null}`:   0,
		`{"max_amount":5000}`:   5000,
		`{"max_amount":1234.5}`: 1234.5,
	}
	for constraints, want := range cases {
		t.Run(constraints, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			svc := newCeilingService(t, mockStore)
			stubActorPermissions(ctx, mockStore, adminOnly)
			var saved *Group
			mockStore.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).
				Run(func(args mock.Arguments) { saved, _ = args.Get(1).(*Group) }).Return(nil).Once()

			_, err := svc.CreateGroupAPI(ctx, ceilingActorID, groupBodyWithCap(t, constraints))

			require.NoError(t, err)
			require.NotNil(t, saved)
			assert.Equal(t, want, saved.Permissions[0].Constraints.MaxPurchaseAmount)
		})
	}
}

// The response type must not grow a wire field for the decode-only flag.
func TestAPIPermissionConstraint_MarshalOmitsUnsetCap(t *testing.T) {
	var c APIPermissionConstraint
	require.NoError(t, json.Unmarshal([]byte(`{"max_amount":0,"regions":["us-east-1"]}`), &c))
	b, err := json.Marshal(c)
	require.NoError(t, err)
	assert.JSONEq(t, `{"regions":["us-east-1"]}`, string(b))
}
