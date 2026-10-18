package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	marketplaceHostKeyID   = "AKIAHOSTAMBIENT0000"
	marketplaceMemberKeyID = "AKIAMEMBERACCOUNT00"
	marketplaceHostAccount = "111111111111"
)

// accountKeyStore returns the member-account access key pair for every account.
type accountKeyStore struct{ MockCredentialStore }

func (*accountKeyStore) LoadRaw(_ context.Context, _, _ string) ([]byte, error) {
	return []byte(`{"access_key_id":"` + marketplaceMemberKeyID + `","secret_access_key":"member-secret"}`), nil
}

// hostSTSTransport answers every STS call as the host account.
type hostSTSTransport struct{}

func (hostSTSTransport) Do(*http.Request) (*http.Response, error) {
	const body = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
		`<Arn>arn:aws:iam::` + marketplaceHostAccount + `:role/host</Arn><UserId>U</UserId><Account>` + marketplaceHostAccount +
		`</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// marketplaceAccountHandler wires a marketplace handler whose ambient AWS
// credentials belong to the host account. The returned pointer receives the
// config the EC2 client was built from.
func marketplaceAccountHandler(t *testing.T, account *config.CloudAccount, row *config.PurchaseHistoryRecord) (*Handler, *aws.Config) {
	t.Helper()
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	cfgStore.On("GetCloudAccount", mock.Anything, "acct-1").Return(account, nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil).Maybe()
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(nil).Maybe()
	cfgStore.On("ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	var built aws.Config
	h := &Handler{
		config:    cfgStore,
		credStore: &accountKeyStore{},
		auth:      authSvc,
		marketplaceEC2Factory: func(cfg aws.Config) marketplaceEC2Client {
			built = cfg
			return &stubMarketplaceEC2{}
		},
	}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Region:      "us-east-1",
			Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
			HTTPClient:  hostSTSTransport{},
		}
	})
	return h, &built
}

func memberAccount() *config.CloudAccount {
	return &config.CloudAccount{ID: "acct-1", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "access_keys"}
}

func signingKeyID(t *testing.T, cfg aws.Config) string {
	t.Helper()
	require.NotNil(t, cfg.Credentials, "EC2 client was built without credentials")
	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	return creds.AccessKeyID
}

func TestMarketplaceList_UsesPurchaseAccountCredentials(t *testing.T) {
	h, built := marketplaceAccountHandler(t, memberAccount(), standardRow())

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	assert.Equal(t, marketplaceMemberKeyID, signingKeyID(t, *built), "listing must be issued with the purchase account's credentials, not the host's")
	assert.Equal(t, "us-east-1", built.Region)
}

func TestMarketplaceCancel_UsesPurchaseAccountCredentials(t *testing.T) {
	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-cancel"
	h, built := marketplaceAccountHandler(t, memberAccount(), row)

	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	assert.Equal(t, marketplaceMemberKeyID, signingKeyID(t, *built), "cancel must be issued with the purchase account's credentials, not the host's")
}

func TestMarketplaceList_RoleARNAccountWithoutRoleRefusedWhenNotHost(t *testing.T) {
	account := &config.CloudAccount{ID: "acct-1", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "role_arn"}
	h, built := marketplaceAccountHandler(t, account, standardRow())

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Equal(t, credentials.ErrNotHostAccount.Error(), ce.message)
	assert.NotContains(t, ce.message, marketplaceHostAccount)
	assert.Nil(t, built.Credentials, "no EC2 client may be built with the host's credentials for a non-host account")
}

func TestMarketplaceList_RoleARNAccountWithoutRoleAllowedForHost(t *testing.T) {
	account := &config.CloudAccount{ID: "acct-1", Provider: "aws", ExternalID: marketplaceHostAccount, AWSAuthMode: "role_arn"}
	h, built := marketplaceAccountHandler(t, account, standardRow())

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	assert.Equal(t, marketplaceHostKeyID, signingKeyID(t, *built))
}

const (
	marketplaceTenantRoleARN     = "arn:aws:iam::222222222222:role/tenant"
	marketplaceAssumedKeyID      = "ASIAASSUMEDMEMBER00"
	marketplaceAssumeRoleDenyXML = `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>User: arn:aws:sts::111111111111:assumed-role/host/session is not authorized to perform: sts:AssumeRole on resource: ` + marketplaceTenantRoleARN + `</Message></Error><RequestId>r</RequestId></ErrorResponse>`
	marketplaceAssumeRoleOKXML   = `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>` + marketplaceAssumedKeyID +
		`</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>` +
		`<AssumedRoleUser><AssumedRoleId>A:s</AssumedRoleId><Arn>arn:aws:sts::222222222222:assumed-role/tenant/s</Arn></AssumedRoleUser></AssumeRoleResult>` +
		`<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></AssumeRoleResponse>`
)

// assumeRoleSTS answers AssumeRole with the given status and body and every
// other STS call as the host account.
type assumeRoleSTS struct {
	status int
	body   string
}

func (a assumeRoleSTS) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), "Action=AssumeRole") {
		return &http.Response{StatusCode: a.status, Body: io.NopCloser(strings.NewReader(a.body)), Header: http.Header{}}, nil
	}
	return hostSTSTransport{}.Do(r)
}

func roleAccount() *config.CloudAccount {
	return &config.CloudAccount{ID: "acct-1", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "role_arn", AWSRoleARN: marketplaceTenantRoleARN}
}

// realEC2Handler builds the EC2 client with the real SDK so the lazy
// credential retrieval (and its error text) is exercised.
func realEC2Handler(t *testing.T, row *config.PurchaseHistoryRecord, sts assumeRoleSTS) *Handler {
	t.Helper()
	h, _ := marketplaceAccountHandler(t, roleAccount(), row)
	h.awsCfg.HTTPClient = sts
	h.marketplaceEC2Factory = func(cfg aws.Config) marketplaceEC2Client { return awsprovider.NewEC2ClientDirect(cfg) }
	return h
}

func assertTenantSafeRoleDenial(t *testing.T, err error) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Equal(t, marketplaceRoleAssumptionMessage, ce.message)
	assert.NotContains(t, ce.message, marketplaceHostAccount)
	assert.NotContains(t, ce.message, "arn:aws:")
}

func TestMarketplaceList_AssumeRoleDeniedReturnsFixedMessage(t *testing.T) {
	h := realEC2Handler(t, standardRow(), assumeRoleSTS{403, marketplaceAssumeRoleDenyXML})

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	assertTenantSafeRoleDenial(t, err)
}

func TestMarketplaceCancel_AssumeRoleDeniedReturnsFixedMessage(t *testing.T) {
	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-cancel"
	h := realEC2Handler(t, row, assumeRoleSTS{403, marketplaceAssumeRoleDenyXML})

	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	assertTenantSafeRoleDenial(t, err)
}

// deniedCancelEC2 creates listings through the stub but cancels through the
// real client, whose credentials fail to resolve.
type deniedCancelEC2 struct {
	*stubMarketplaceEC2
	real marketplaceEC2Client
}

func (d deniedCancelEC2) CancelMarketplaceListing(ctx context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
	return d.real.CancelMarketplaceListing(ctx, id)
}

func TestMarketplaceList_CompensationAssumeRoleDeniedReturnsFixedMessage(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(authSvc)
	row := standardRow()
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	cfgStore.On("GetCloudAccount", mock.Anything, "acct-1").Return(roleAccount(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(errors.New("db down"))
	h := &Handler{config: cfgStore, credStore: &accountKeyStore{}, auth: authSvc}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Region:      "us-east-1",
			Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
			HTTPClient:  assumeRoleSTS{403, marketplaceAssumeRoleDenyXML},
		}
	})
	h.marketplaceEC2Factory = func(cfg aws.Config) marketplaceEC2Client {
		return deniedCancelEC2{&stubMarketplaceEC2{}, awsprovider.NewEC2ClientDirect(cfg)}
	}

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 502, ce.code)
	assert.Contains(t, ce.message, marketplaceRoleAssumptionMessage)
	assert.NotContains(t, ce.message, marketplaceHostAccount)
	assert.NotContains(t, ce.message, "arn:aws:")
}

func TestMarketplaceList_RoleARNAccountAssumesRole(t *testing.T) {
	h, built := marketplaceAccountHandler(t, roleAccount(), standardRow())
	h.awsCfg.HTTPClient = assumeRoleSTS{200, marketplaceAssumeRoleOKXML}

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	assert.Equal(t, marketplaceAssumedKeyID, signingKeyID(t, *built))
}

func TestMarketplaceCancel_RoleARNAccountAssumesRole(t *testing.T) {
	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-cancel"
	h, built := marketplaceAccountHandler(t, roleAccount(), row)
	h.awsCfg.HTTPClient = assumeRoleSTS{200, marketplaceAssumeRoleOKXML}

	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	assert.Equal(t, marketplaceAssumedKeyID, signingKeyID(t, *built))
}

func TestLoadMarketplaceAWSConfig_FailsClosed(t *testing.T) {
	newHandler := func(account *config.CloudAccount, lookupErr error) *Handler {
		cfgStore := &MockConfigStore{}
		cfgStore.On("GetCloudAccount", mock.Anything, "acct-1").Return(account, lookupErr)
		h := &Handler{config: cfgStore, credStore: &accountKeyStore{}}
		h.awsCfgOnce.Do(func() {
			h.awsCfg = aws.Config{
				Region:      "us-east-1",
				Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
				HTTPClient:  hostSTSTransport{},
			}
		})
		return h
	}

	t.Run("nil account is not host credentials", func(t *testing.T) {
		cfg, err := newHandler(nil, nil).loadMarketplaceAWSConfig(context.Background(), standardRow())

		ce, ok := IsClientError(err)
		require.True(t, ok, "expected a client error, got %v", err)
		assert.Equal(t, 400, ce.code)
		assert.Contains(t, ce.message, "no longer exists")
		assert.Nil(t, cfg.Credentials)
	})

	t.Run("lookup error is not host credentials", func(t *testing.T) {
		cfg, err := newHandler(nil, errors.New("db down")).loadMarketplaceAWSConfig(context.Background(), standardRow())

		require.Error(t, err)
		_, isClient := IsClientError(err)
		assert.False(t, isClient)
		assert.Nil(t, cfg.Credentials)
	})

	t.Run("non-aws provider", func(t *testing.T) {
		account := &config.CloudAccount{ID: "acct-1", Provider: "azure"}
		cfg, err := newHandler(account, nil).loadMarketplaceAWSConfig(context.Background(), standardRow())

		ce, ok := IsClientError(err)
		require.True(t, ok, "expected a client error, got %v", err)
		assert.Equal(t, 400, ce.code)
		assert.Nil(t, cfg.Credentials)
	})
}

// countingSTS records how many STS calls were made and answers them with the
// wrapped transport.
type countingSTS struct {
	calls *int
	next  aws.HTTPClient
}

func (c countingSTS) Do(r *http.Request) (*http.Response, error) {
	*c.calls++
	return c.next.Do(r)
}

// failingSTS fails every STS call at the transport.
type failingSTS struct{}

func (failingSTS) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("sts unreachable")
}

// noCloudAccountRow is a purchase row with no cloud account (ambient
// single-account deployment, legacy or account-deleted row) owned by the given
// AWS account.
func noCloudAccountRow(cloudAccountID *string, awsAccountID string) *config.PurchaseHistoryRecord {
	row := standardRow()
	row.CloudAccountID = cloudAccountID
	row.AccountID = awsAccountID
	return row
}

func hostOnlyHandler(transport aws.HTTPClient) *Handler {
	// No GetCloudAccount expectation: a row without a cloud account must not
	// look one up (the mock panics on an unexpected call).
	h := &Handler{config: &MockConfigStore{}}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Region:      "us-east-1",
			Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
			HTTPClient:  transport,
		}
	})
	return h
}

// A row with no cloud account gets the host's credentials only when its AWS
// account id is the host account (issue #261).
func TestLoadMarketplaceAWSConfig_NoCloudAccountHostRowUsesHostCredentials(t *testing.T) {
	empty, blank := "", "  "
	for name, id := range map[string]*string{"nil": nil, "empty": &empty, "blank": &blank} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			h := hostOnlyHandler(countingSTS{&calls, hostSTSTransport{}})

			cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(id, marketplaceHostAccount))

			require.NoError(t, err)
			assert.Equal(t, marketplaceHostKeyID, signingKeyID(t, cfg))
			assert.Equal(t, 1, calls, "the host identity must be checked before the host's credentials are used")
		})
	}
}

func TestLoadMarketplaceAWSConfig_NoCloudAccountRefusedUnlessHost(t *testing.T) {
	cases := map[string]struct {
		owner     string
		transport aws.HTTPClient
		wantSTS   bool
	}{
		"member account":     {owner: "222222222222", transport: hostSTSTransport{}, wantSTS: true},
		"empty account id":   {owner: "", transport: hostSTSTransport{}, wantSTS: false},
		"blank account id":   {owner: "   ", transport: hostSTSTransport{}, wantSTS: false},
		"host identity fail": {owner: marketplaceHostAccount, transport: failingSTS{}, wantSTS: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			h := hostOnlyHandler(countingSTS{&calls, tc.transport})
			h.awsCfg.Retryer = noRetry

			cfg, err := h.loadMarketplaceAWSConfig(context.Background(), noCloudAccountRow(nil, tc.owner))

			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, marketplaceNoCloudAccountMessage, ce.message)
			assert.NotContains(t, ce.message, marketplaceHostAccount)
			assert.Nil(t, cfg.Credentials, "a refused row must not get the host's credentials")
			assert.Equal(t, tc.wantSTS, calls > 0)
		})
	}
}

// End to end through both handlers: a no-cloud-account row owned by a member
// account never reaches EC2, and one owned by the host account is signed with
// the host's credentials.
func TestMarketplaceHandlers_NoCloudAccountRow(t *testing.T) {
	run := map[string]func(*Handler) (any, error){
		"list": func(h *Handler) (any, error) {
			return h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
		},
		"cancel": func(h *Handler) (any, error) {
			return h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
		},
	}
	// A row with no cloud account is reachable only by an unrestricted
	// sell-any caller; authorization itself is unchanged.
	unrestricted := func(h *Handler) {
		h.auth.(*MockAuthService).On("HasPermissionForConstraintsAPI", mock.Anything, "admin", auth.ActionSellAny, auth.ResourcePurchases,
			[]auth.PermissionConstraints{{StrictScope: true, Regions: []string{"us-east-1"}}}).Return(true, nil)
	}
	for name, call := range run {
		row := func(accountID string) *config.PurchaseHistoryRecord {
			r := noCloudAccountRow(nil, accountID)
			if name == "cancel" {
				r.ListingState = config.ListingStateActive
				r.ListingID = "ril-cancel"
			}
			return r
		}
		t.Run(name+"/member account refused", func(t *testing.T) {
			h, built := marketplaceAccountHandler(t, nil, row("222222222222"))
			unrestricted(h)
			built.Region = "unset"

			_, err := call(h)

			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, marketplaceNoCloudAccountMessage, ce.message)
			assert.Equal(t, "unset", built.Region, "no EC2 client may be built for a refused row")
			store := h.config.(*MockConfigStore)
			store.AssertNotCalled(t, "GetCloudAccount", mock.Anything, mock.Anything)
		})
		t.Run(name+"/host account uses host credentials", func(t *testing.T) {
			h, built := marketplaceAccountHandler(t, nil, row(marketplaceHostAccount))
			unrestricted(h)

			_, err := call(h)

			require.NoError(t, err)
			assert.Equal(t, marketplaceHostKeyID, signingKeyID(t, *built))
			store := h.config.(*MockConfigStore)
			store.AssertNotCalled(t, "GetCloudAccount", mock.Anything, mock.Anything)
		})
	}
}

func TestMapAWSMarketplaceError_GenuineEC2ErrorsKeepTheirOwnMessage(t *testing.T) {
	for _, code := range []string{"InvalidReservedInstancesId.NotFound", "UnauthorizedOperation"} {
		t.Run(code, func(t *testing.T) {
			const msg = "the ec2 service said no"
			err := &smithy.OperationError{
				ServiceID:     "EC2",
				OperationName: "CreateReservedInstancesListing",
				Err:           &smithy.GenericAPIError{Code: code, Message: msg, Fault: smithy.FaultClient},
			}

			ce, ok := IsClientError(mapAWSMarketplaceError("create listing", err))

			require.True(t, ok)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, msg, ce.message)
			assert.NotEqual(t, marketplaceRoleAssumptionMessage, ce.message)
		})
	}
}

// loadErrStore fails the credential load with an arbitrary error.
type loadErrStore struct{ MockCredentialStore }

func (*loadErrStore) LoadRaw(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("secret backend unavailable")
}

// missingKeyStore returns a credential payload without an access key.
type missingKeyStore struct{ MockCredentialStore }

func (*missingKeyStore) LoadRaw(context.Context, string, string) ([]byte, error) {
	return []byte(`{"secret_access_key":"member-secret"}`), nil
}

func TestLoadMarketplaceAWSConfig_ResolverErrorNeverFallsBackToHost(t *testing.T) {
	stores := map[string]credentials.CredentialStore{
		"store error":        &loadErrStore{},
		"missing access key": &missingKeyStore{},
	}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			cfgStore := &MockConfigStore{}
			cfgStore.On("GetCloudAccount", mock.Anything, "acct-1").Return(memberAccount(), nil)
			h := &Handler{config: cfgStore, credStore: store}
			h.awsCfgOnce.Do(func() {
				h.awsCfg = aws.Config{
					Region:      "us-east-1",
					Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
					HTTPClient:  hostSTSTransport{},
				}
			})

			cfg, err := h.loadMarketplaceAWSConfig(context.Background(), standardRow())

			require.Error(t, err)
			assert.False(t, errors.Is(err, credentials.ErrNotHostAccount))
			assert.Nil(t, cfg.Credentials)
		})
	}
}
