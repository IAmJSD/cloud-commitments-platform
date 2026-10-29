package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSessionPurchaseActionSelection(t *testing.T) {
	lookupErr := errors.New("permission lookup failed")
	for _, actions := range [][2]string{{auth.ActionSellAny, auth.ActionSellOwn}, {auth.ActionRevokeAny, auth.ActionRevokeOwn}} {
		for _, failure := range []error{nil, lookupErr} {
			service := new(MockAuthService)
			service.On("ValidateSession", mock.Anything, "test-token").Return(&Session{UserID: "owner"}, nil)
			service.On("HasPermissionAPI", mock.Anything, "owner", actions[0], auth.ResourcePurchases).Return(false, failure)
			if failure == nil {
				service.On("HasPermissionAPI", mock.Anything, "owner", actions[1], auth.ResourcePurchases).Return(true, nil)
			}
			handler := &Handler{auth: service}
			session, action, err := handler.requireSessionPurchaseAction(context.Background(), marketplaceReq(), actions[0], actions[1])
			if failure != nil {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
				require.Equal(t, actions[1], action)
				require.Equal(t, "owner", session.UserID)
			}
			service.AssertExpectations(t)
		}
	}
}

func TestMarketplaceScopeLookupErrors(t *testing.T) {
	lookupErr := errors.New("scope lookup failed")
	for _, cancel := range []bool{false, true} {
		for _, accountFailure := range []bool{false, true} {
			service, store, provider := new(MockAuthService), new(MockConfigStore), new(stubMarketplaceEC2)
			t.Cleanup(func() { store.AssertExpectations(t) })
			service.On("ValidateSession", mock.Anything, "test-token").Return(&Session{UserID: "owner"}, nil)
			service.On("HasPermissionAPI", mock.Anything, "owner", auth.ActionSellAny, auth.ResourcePurchases).Return(true, nil)
			if accountFailure {
				service.On("GetAllowedAccountsAPI", mock.Anything, "owner").Return([]string(nil), lookupErr)
			} else {
				service.On("GetAllowedAccountsAPI", mock.Anything, "owner").Return([]string{"*"}, nil)
				service.On("HasPermissionForConstraintsAPI", mock.Anything, "owner", auth.ActionSellAny, auth.ResourcePurchases,
					[]auth.PermissionConstraints{{StrictScope: true, AccountIDs: []string{"acct-1"}, Regions: []string{"us-east-1"}}}).Return(false, lookupErr)
			}
			store.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(standardRow(), nil)
			handler := newMarketplaceHandler(store, service, provider)
			run := handler.marketplaceList
			if cancel {
				run = handler.marketplaceCancel
			}
			_, err := run(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
			require.ErrorIs(t, err, lookupErr)
			require.Zero(t, provider.createCallCount)
			require.Zero(t, provider.cancelCallCount)
			store.AssertNotCalled(t, "ClaimMarketplaceListingSlot", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "UpdatePurchaseHistoryListing", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			service.AssertExpectations(t)
		}
	}
}
