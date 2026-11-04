package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Unit tests for approveRegistration after issue #1102 item 2. Approval used
// to flip the registration to approved, then create the account in a separate
// statement, so a failed create left an approved registration with no account.
// The handler now makes one ApproveAccountRegistration call, which does all of
// it in a transaction; the real-Postgres atomicity and race tests are in
// handler_registrations_approve_integration_test.go.

const approveRegID = "5a5a5a5a-1102-4102-8102-000000000001"

const approveRegBody = `{"name":"Fixture Sub","provider":"azure","external_id":"fixture-subscription-1102","azure_auth_mode":"managed_identity"}`

func pendingRegistrationFixture() *config.AccountRegistration {
	return &config.AccountRegistration{
		ID: approveRegID, Status: "pending", Provider: "azure",
		ExternalID: "fixture-subscription-1102", AccountName: "Fixture Sub",
	}
}

// newApproveRegistrationHandler wires an admin session and a pending
// registration. The pre-#1102 steps are registered with .Maybe() so a
// regression to them fails the AssertNotCalled checks instead of panicking.
func newApproveRegistrationHandler(t *testing.T, ctx context.Context, approveErr error) (*Handler, *MockConfigStore) {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetAccountRegistration", ctx, approveRegID).Return(pendingRegistrationFixture(), nil)
	store.On("ApproveAccountRegistration", ctx, mock.Anything, mock.Anything, mock.Anything).Return(approveErr)
	store.On("TransitionRegistrationStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("CreateCloudAccount", mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("UpdateAccountRegistration", mock.Anything, mock.Anything).Return(nil).Maybe()
	return &Handler{auth: mockAuth, config: store}, store
}

// assertNoPiecemealApproval checks that none of the separate, non-atomic
// steps the handler used before #1102 ran.
func assertNoPiecemealApproval(t *testing.T, store *MockConfigStore) {
	t.Helper()
	store.AssertNotCalled(t, "TransitionRegistrationStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "CreateCloudAccount", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "UpdateAccountRegistration", mock.Anything, mock.Anything)
}

func TestApproveRegistration_ApprovesAndCreatesInOneStoreCall(t *testing.T) {
	ctx := context.Background()
	h, store := newApproveRegistrationHandler(t, ctx, nil)

	result, err := h.approveRegistration(ctx, adminRequest(approveRegBody), approveRegID)
	require.NoError(t, err)
	account, ok := result.(*config.CloudAccount)
	require.True(t, ok, "got %T", result)

	store.AssertCalled(t, "ApproveAccountRegistration", ctx,
		mock.MatchedBy(func(r *config.AccountRegistration) bool {
			return r.ID == approveRegID && r.ReviewedAt != nil && r.ReviewedBy != nil &&
				*r.ReviewedBy == adminAccountSession().UserID
		}),
		mock.MatchedBy(func(a *config.CloudAccount) bool {
			return a.ID == account.ID && a.Provider == "azure" &&
				a.ExternalID == "fixture-subscription-1102" && a.Enabled
		}),
		mock.MatchedBy(func(actor *string) bool { return actor != nil && *actor == adminAccountSession().UserID }),
	)
	assertNoPiecemealApproval(t, store)
}

func TestApproveRegistration_StoreErrorsMapToResponses(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		code     int // 0: not a client error (500)
		message  string
	}{
		{"already decided", config.ErrRegistrationConflict, 409, "already processed"},
		{"account already registered", fmt.Errorf("failed to create cloud account: %w",
			errors.New(`ERROR: duplicate key value violates unique constraint "cloud_accounts_provider_external_id_key" (SQLSTATE 23505)`)),
			409, `external ID "fixture-subscription-1102" already exists for azure`},
		{"database failure", errors.New("connection reset"), 0, "registrations: approve: connection reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, store := newApproveRegistrationHandler(t, ctx, tc.storeErr)

			result, err := h.approveRegistration(ctx, adminRequest(approveRegBody), approveRegID)
			assert.Nil(t, result)
			require.Error(t, err)
			ce, ok := IsClientError(err)
			if tc.code == 0 {
				require.False(t, ok, "a database failure must stay a 500, got client error %v", err)
				assert.ErrorContains(t, err, tc.message)
			} else {
				require.True(t, ok, "want a client error, got %v", err)
				assert.Equal(t, tc.code, ce.code)
				assert.Contains(t, ce.message, tc.message)
			}
			assertNoPiecemealApproval(t, store)
		})
	}
}
