package auth

// Negative MaxPurchaseAmount must fail closed (issue #83).
//
// Zero is the unset zero-value and keeps meaning "no cap". A negative (or
// NaN) cap is malformed spend-cap data: enforcement denies every request
// against it, the grant ceiling treats it as covering nothing, and the group
// and API-key write paths refuse it.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMatchPurchaseAmountConstraint_NegativeCapDeniesAll(t *testing.T) {
	malformed := []float64{-1, -0.01, -math.MaxFloat64, math.Inf(-1), math.NaN()}
	requests := []float64{0, 0.01, 1, 1_000_000, math.MaxFloat64, -5}
	for _, permMax := range malformed {
		for _, reqMax := range requests {
			assert.False(t, matchPurchaseAmountConstraint(permMax, reqMax),
				"cap %v must deny request %v", permMax, reqMax)
		}
	}
}

func TestMatchPurchaseAmountConstraint_ValidCapsUnchanged(t *testing.T) {
	cases := []struct {
		permMax, reqMax float64
		want            bool
	}{
		{1000, 500, true},
		{1000, 1000, true},
		{1000, 1000.01, false},
		{1000, math.MaxFloat64, false},
		// Zero keeps its existing meaning: no cap.
		{0, 0, true},
		{0, 1_000_000, true},
		{0, math.MaxFloat64, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, matchPurchaseAmountConstraint(tc.permMax, tc.reqMax),
			"cap %v, request %v", tc.permMax, tc.reqMax)
	}
}

// A malformed REQUESTED amount (negative, NaN or +/-Inf) is denied whenever a
// cap applies. reqMax is computed server-side, so this is defense in depth: a
// NaN in particular compares false against the cap and used to slip under it.
// With no cap (permMax == 0) the amount dimension is unrestricted, so the
// request is allowed, consistent with a permission that carries no
// constraints at all (checkPermissionConstraints never reaches the matcher).
func TestMatchPurchaseAmountConstraint_MalformedRequestedAmount(t *testing.T) {
	malformed := []float64{-1, -0.01, -math.MaxFloat64, math.Inf(-1), math.Inf(1), math.NaN()}
	for _, reqMax := range malformed {
		assert.False(t, matchPurchaseAmountConstraint(1000, reqMax),
			"capped permission must deny malformed request %v", reqMax)
		assert.False(t, matchPurchaseAmountConstraint(math.MaxFloat64, reqMax),
			"capped permission must deny malformed request %v even at the largest cap", reqMax)
		assert.True(t, matchPurchaseAmountConstraint(0, reqMax),
			"uncapped permission leaves the amount dimension unrestricted for %v", reqMax)
	}
}

// Through the real service enforcement path a handler calls at purchase and
// approval time: a group permission stored with a negative cap refuses even a
// $1 purchase, while a sibling positive cap is enforced exactly as before.
func TestService_HasPermissionForConstraintsAPI_NegativeStoredCap(t *testing.T) {
	ctx := context.Background()
	stub := func(mockStore *MockStore, maxAmount float64) {
		mockStore.On("GetUserByID", ctx, "purchaser-1").
			Return(&User{ID: "purchaser-1", GroupIDs: []string{"purchasers"}}, nil).Once()
		mockStore.On("GetGroup", ctx, "purchasers").Return(&Group{
			ID: "purchasers",
			Permissions: []Permission{{
				Action:      ActionExecute,
				Resource:    ResourcePurchases,
				Constraints: &PermissionConstraints{Providers: []string{"aws"}, MaxPurchaseAmount: maxAmount},
			}},
		}, nil).Once()
	}
	cases := []struct {
		name    string
		cap     float64
		request float64
		want    bool
	}{
		{"negative cap refuses a small purchase", -100, 1, false},
		{"negative cap refuses a large purchase", -100, 1_000_000, false},
		{"positive cap allows within cap", 1000, 500, true},
		{"positive cap refuses over cap", 1000, 5000, false},
		{"zero cap is no cap", 0, 1_000_000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			svc := createTestService(mockStore, new(MockEmailSender))
			stub(mockStore, tc.cap)

			has, err := svc.HasPermissionForConstraintsAPI(ctx, "purchaser-1", ActionExecute, ResourcePurchases,
				[]PermissionConstraints{{Providers: []string{"aws"}, MaxPurchaseAmount: tc.request}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, has)
		})
	}
}

func TestAmountCovers_NegativeHeldCapCoversNothing(t *testing.T) {
	for _, held := range []float64{-1, -0.01, math.NaN()} {
		for _, req := range []float64{0, 1, 100, -1} {
			assert.False(t, amountCovers(held, req), "held %v must not cover %v", held, req)
		}
	}
	// Unchanged for well-formed caps.
	assert.True(t, amountCovers(0, 0))
	assert.True(t, amountCovers(0, 1_000_000))
	assert.True(t, amountCovers(100, 50))
	assert.True(t, amountCovers(100, 100))
	assert.False(t, amountCovers(100, 101))
	assert.False(t, amountCovers(100, 0))
}

// An already-stored negative cap on a carved-out money verb must not be
// carried through an edit as an UNCAPPED permission: before the fix the
// carry-over check (permissionCoveredBy -> amountCovers) read the negative
// cap as "no cap" and accepted the widening.
func TestGrantCeiling_StoredNegativeCapCannotBeWidened(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, adminOnly)
	stubTargetGroup(ctx, mockStore, &Group{
		ID:   ceilingTargetID,
		Name: "Custom Purchasers",
		Permissions: []Permission{{
			Action:      ActionExecute,
			Resource:    ResourcePurchases,
			Constraints: &PermissionConstraints{MaxPurchaseAmount: -1},
		}},
	})
	mockStore.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()

	_, err := svc.UpdateGroupAPI(ctx, ceilingActorID, ceilingTargetID,
		updateReqWith(APIPermission{Action: ActionExecute, Resource: ResourcePurchases}))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionNotGrantable)
	mockStore.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
}

func TestGroupWrite_RejectsNegativeMaxAmount(t *testing.T) {
	ctx := context.Background()
	for _, amount := range []float64{-1, -0.01} {
		perm := APIPermission{Action: ActionView, Resource: ResourcePlans,
			Constraints: &APIPermissionConstraint{MaxAmount: amount}}

		t.Run("update", func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			svc := newCeilingService(t, mockStore)
			stubTargetGroup(ctx, mockStore, &Group{ID: ceilingTargetID, Name: "Team"})
			// Permissive stubs: without the guard the write would succeed.
			stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
			mockStore.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()

			_, err := svc.UpdateGroupAPI(ctx, ceilingActorID, ceilingTargetID, updateReqWith(perm))

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPermission)
			assert.Contains(t, err.Error(), "entry 0")
			assert.Contains(t, err.Error(), "max purchase amount")
			mockStore.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
		})

		t.Run("create", func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			svc := newCeilingService(t, mockStore)
			stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
			mockStore.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()

			_, err := svc.CreateGroupAPI(ctx, ceilingActorID, APICreateGroupRequest{
				Name: "Team", Permissions: []APIPermission{perm},
			})

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPermission)
			mockStore.AssertNotCalled(t, "CreateGroup", mock.Anything, mock.Anything)
		})
	}
}

// Negative control: zero (no cap) and a positive cap are still written.
func TestGroupWrite_AcceptsZeroAndPositiveMaxAmount(t *testing.T) {
	ctx := context.Background()
	for _, amount := range []float64{0, 0.01, 5000} {
		mockStore := new(MockStore)
		svc := newCeilingService(t, mockStore)
		stubActorPermissions(ctx, mockStore, adminOnly)
		var saved *Group
		mockStore.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).
			Run(func(args mock.Arguments) { saved, _ = args.Get(1).(*Group) }).Return(nil).Once()

		_, err := svc.CreateGroupAPI(ctx, ceilingActorID, APICreateGroupRequest{
			Name: "Team",
			Permissions: []APIPermission{{Action: ActionView, Resource: ResourcePlans,
				Constraints: &APIPermissionConstraint{MaxAmount: amount}}},
		})

		require.NoError(t, err, "amount %v", amount)
		require.NotNil(t, saved)
		assert.Equal(t, amount, saved.Permissions[0].Constraints.MaxPurchaseAmount)
		mockStore.AssertExpectations(t)
	}
}

func TestCreateAPIKey_NegativeMaxPurchaseAmount(t *testing.T) {
	ctx := context.Background()
	passwordHash, err := newTestService().hashPassword(testAPIKeyPassword)
	require.NoError(t, err)
	expiresAt := time.Now().Add(24 * time.Hour)
	grpID := "99999999-9999-4999-8999-999999999999"

	run := func(mockStore *MockStore, amount float64) error {
		service := &Service{store: mockStore}
		user := &User{ID: "user-123", Active: true, PasswordHash: passwordHash, GroupIDs: []string{grpID}}
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, grpID).Return(&Group{
			ID:          grpID,
			Permissions: []Permission{{Action: ActionView, Resource: ResourcePlans}},
		}, nil).Maybe()
		mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(nil).Maybe()

		_, _, err := service.CreateAPIKey(ctx, "user-123", "ci", testAPIKeyPassword, []Permission{
			{Action: ActionView, Resource: ResourcePlans},
			{Action: ActionView, Resource: ResourcePlans, Constraints: &PermissionConstraints{MaxPurchaseAmount: amount}},
		}, &expiresAt)
		return err
	}

	t.Run("negative refused before the key is stored", func(t *testing.T) {
		mockStore := new(MockStore)
		err := run(mockStore, -1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidPermission)
		assert.Contains(t, err.Error(), "entry 1")
		mockStore.AssertNotCalled(t, "CreateAPIKey", mock.Anything, mock.Anything)
	})

	for _, amount := range []float64{0, 100} {
		mockStore := new(MockStore)
		require.NoError(t, run(mockStore, amount), "amount %v", amount)
		mockStore.AssertCalled(t, "CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey"))
	}
}
