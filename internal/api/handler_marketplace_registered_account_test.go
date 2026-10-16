package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #560: a purchase row with no cloud account (legacy, or its account
// was deleted and the FK set it NULL) is resolved by (aws, row AccountID) to
// the single registered AWS account with that external id, and is signed with
// that account's credentials instead of being refused or signed as the host.

const marketplaceMemberAccount = "222222222222"

// registeredMember is a registered access-key AWS account that is not the
// host account. Its id differs from standardRow's "acct-1" so a test can tell
// the external-id lookup apart from a GetCloudAccount lookup.
func registeredMember() config.CloudAccount {
	return config.CloudAccount{ID: "acct-member", Provider: "aws", ExternalID: marketplaceMemberAccount, AWSAuthMode: "access_keys"}
}

// withRegisteredAccounts makes ListCloudAccounts return accounts (or err).
func withRegisteredAccounts(h *Handler, accounts []config.CloudAccount, err error) {
	h.config.(*MockConfigStore).ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, err
	}
}

// registeredLookupHandler is hostOnlyHandler with the member access keys
// available and the given registered accounts.
func registeredLookupHandler(transport aws.HTTPClient, accounts []config.CloudAccount, listErr error) *Handler {
	h := hostOnlyHandler(transport)
	h.credStore = &accountKeyStore{}
	withRegisteredAccounts(h, accounts, listErr)
	return h
}

func TestLoadMarketplaceAWSConfig_NoCloudAccountResolvesRegisteredAccount(t *testing.T) {
	empty := ""
	for name, id := range map[string]*string{"nil": nil, "empty": &empty} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			h := registeredLookupHandler(countingSTS{&calls, hostSTSTransport{}}, []config.CloudAccount{registeredMember()}, nil)

			cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(id, marketplaceMemberAccount))

			require.NoError(t, err)
			assert.Equal(t, marketplaceMemberKeyID, signingKeyID(t, cfg), "a registered non-host account must sign with its own credentials")
			assert.Zero(t, calls, "an access-key account needs no host identity check")
			store := h.config.(*MockConfigStore)
			store.AssertNotCalled(t, "GetCloudAccount", mock.Anything, mock.Anything)
		})
	}
}

// A registered role_arn account assumes its own role, so the lookup goes
// through the same resolver as a row with a cloud account.
func TestLoadMarketplaceAWSConfig_NoCloudAccountRegisteredRoleAccountAssumesRole(t *testing.T) {
	h := registeredLookupHandler(assumeRoleSTS{200, marketplaceAssumeRoleOKXML}, []config.CloudAccount{*roleAccount()}, nil)

	cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(nil, marketplaceMemberAccount))

	require.NoError(t, err)
	assert.Equal(t, marketplaceAssumedKeyID, signingKeyID(t, cfg))
}

// A registered role_arn account without a role ARN keeps the resolver's
// host-only rule: a non-host account is refused, never signed as the host.
func TestLoadMarketplaceAWSConfig_NoCloudAccountRegisteredRoleWithoutARNRefused(t *testing.T) {
	account := config.CloudAccount{ID: "acct-member", Provider: "aws", ExternalID: marketplaceMemberAccount, AWSAuthMode: "role_arn"}
	h := registeredLookupHandler(hostSTSTransport{}, []config.CloudAccount{account}, nil)

	cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(nil, marketplaceMemberAccount))

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Equal(t, credentials.ErrNotHostAccount.Error(), ce.message)
	assert.Nil(t, cfg.Credentials)
}

// An id that matches no single registered AWS account keeps #261's rule: the
// host's credentials only for the host account, otherwise a 400.
func TestLoadMarketplaceAWSConfig_NoCloudAccountUnresolvedFallsBackToHostCheck(t *testing.T) {
	member := registeredMember()
	twin := member
	twin.ID = "acct-twin"
	azure := member
	azure.Provider = "azure"
	cases := map[string]struct {
		accounts []config.CloudAccount
		owner    string
		wantHost bool
	}{
		"unregistered member":            {accounts: nil, owner: marketplaceMemberAccount},
		"same external id on azure only": {accounts: []config.CloudAccount{azure}, owner: marketplaceMemberAccount},
		"ambiguous aws accounts":         {accounts: []config.CloudAccount{member, twin}, owner: marketplaceMemberAccount},
		"other account registered":       {accounts: []config.CloudAccount{member}, owner: "333333333333"},
		"unregistered host":              {accounts: []config.CloudAccount{member}, owner: marketplaceHostAccount, wantHost: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := registeredLookupHandler(hostSTSTransport{}, tc.accounts, nil)

			cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(nil, tc.owner))

			if tc.wantHost {
				require.NoError(t, err)
				assert.Equal(t, marketplaceHostKeyID, signingKeyID(t, cfg))
				return
			}
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, marketplaceNoCloudAccountMessage, ce.message)
			assert.Nil(t, cfg.Credentials)
		})
	}
}

// A failed account lookup is a server error, not a fallback to the host.
func TestLoadMarketplaceAWSConfig_NoCloudAccountLookupErrorFailsClosed(t *testing.T) {
	calls := 0
	h := registeredLookupHandler(countingSTS{&calls, hostSTSTransport{}}, nil, errors.New("db down"))

	cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(nil, marketplaceHostAccount))

	require.Error(t, err)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
	assert.Contains(t, err.Error(), "db down")
	assert.Nil(t, cfg.Credentials)
	assert.Zero(t, calls)
}

// End to end through both handlers: a no-cloud-account row owned by a
// registered member account is listed and canceled with that account's keys.
func TestMarketplaceHandlers_NoCloudAccountRegisteredRow(t *testing.T) {
	run := map[string]func(*Handler) (any, error){
		"list": func(h *Handler) (any, error) {
			return h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
		},
		"cancel": func(h *Handler) (any, error) {
			return h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
		},
	}
	for name, call := range run {
		t.Run(name, func(t *testing.T) {
			row := noCloudAccountRow(nil, marketplaceMemberAccount)
			if name == "cancel" {
				row.ListingState = config.ListingStateActive
				row.ListingID = "ril-cancel"
			}
			h, built := marketplaceAccountHandler(t, nil, row)
			h.auth.(*MockAuthService).On("HasPermissionForConstraintsAPI", mock.Anything, "admin", auth.ActionSellAny, auth.ResourcePurchases,
				[]auth.PermissionConstraints{{StrictScope: true, Regions: []string{"us-east-1"}}}).Return(true, nil)
			withRegisteredAccounts(h, []config.CloudAccount{registeredMember()}, nil)

			_, err := call(h)

			require.NoError(t, err)
			assert.Equal(t, marketplaceMemberKeyID, signingKeyID(t, *built))
			store := h.config.(*MockConfigStore)
			store.AssertNotCalled(t, "GetCloudAccount", mock.Anything, mock.Anything)
		})
	}
}
