package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Regression tests for #531: an AWS role_arn account with an empty role ARN
// means "use CUDly's own credentials", so create, update and registration
// approval must reject it unless external_id is the host account, and fail
// closed when the host identity cannot be resolved. The 400 must not name the
// host account or carry STS error text. The fakes reuse probeSTS from
// handler_accounts_awsprobe_test.go: the host keys resolve to probeHostAcct.

const hostCheckRegID = "33333333-3333-3333-3333-333333333333"

// hostCheckHandler returns a handler whose cached host AWS config signs with
// the probe host keys and sends every STS call to fake (no real AWS).
func hostCheckHandler(t *testing.T, store *MockConfigStore, fake aws.HTTPClient, cfgErr error) *Handler {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(context.Background(), mockAuth)
	h := &Handler{auth: mockAuth, config: store}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Credentials:      awscreds.NewStaticCredentialsProvider(probeHostKeyID, "host-secret", ""),
			HTTPClient:       fake,
			RetryMaxAttempts: 1,
		}
		h.awsCfgErr = cfgErr
	})
	return h
}

// stsDoFunc adapts a function to aws.HTTPClient.
type stsDoFunc func(*http.Request) (*http.Response, error)

func (f stsDoFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// hostRejectedSTS answers every call, the host's GetCallerIdentity included,
// with an STS error whose text names the host role and a request id.
var hostRejectedSTS = stsDoFunc(func(*http.Request) (*http.Response, error) {
	return probeError(403, "InvalidClientTokenId", "The security token included in the request is invalid for "+probeHostRoleARN), nil
})

func roleARNBody(externalID, roleARN string) string {
	body := `{"name":"Acme","provider":"aws","external_id":"` + externalID + `","aws_auth_mode":"role_arn"`
	if roleARN != "" {
		body += `,"aws_role_arn":"` + roleARN + `","aws_external_id":"550e8400-e29b-41d4-a716-446655440000"`
	}
	return body + `}`
}

type hostCheckCase struct {
	name       string
	externalID string
	fake       aws.HTTPClient
	cfgErr     error
	wantMsg    string
}

// rejectedHostCheckCases are the shapes every save path must refuse: a
// non-host account, plus the fail-closed STS and host-config failures for
// what would otherwise be the host account. Only the non-host account is told
// to set aws_role_arn; the platform-side failures are not (#559).
func rejectedHostCheckCases() []hostCheckCase {
	return []hostCheckCase{
		{name: "non-host account", externalID: probeTenantAcct, fake: &probeSTS{}, wantMsg: roleARNNotHostMessage},
		{name: "STS unreachable", externalID: probeHostAcct, fake: &probeSTS{transportErr: true}, wantMsg: hostIdentityUnavailableMessage},
		{name: "STS rejects the host credentials", externalID: probeHostAcct, fake: hostRejectedSTS, wantMsg: hostIdentityUnavailableMessage},
		{name: "host AWS config unavailable", externalID: probeHostAcct, fake: &probeSTS{}, cfgErr: errors.New(probeProfileText), wantMsg: hostIdentityUnavailableMessage},
	}
}

func requireNotHost400(t *testing.T, err error, wantMsg string) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Equal(t, wantMsg, ce.message)
	assertNoLeak(t, ce.message, "")
}

func TestHostCheckMessages_OnlyMismatchSuggestsSettingRoleARN(t *testing.T) {
	assert.Contains(t, roleARNNotHostMessage, "set aws_role_arn")
	assert.NotContains(t, hostIdentityUnavailableMessage, "set aws_role_arn")
}

func TestCreateAccount_EmptyRoleARNRequiresHostAccount(t *testing.T) {
	ctx := context.Background()
	for _, tc := range rejectedHostCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			h := hostCheckHandler(t, store, tc.fake, tc.cfgErr)
			result, err := h.createAccount(ctx, adminRequest(roleARNBody(tc.externalID, "")))
			assert.Nil(t, result)
			requireNotHost400(t, err, tc.wantMsg)
			store.AssertNotCalled(t, "CreateCloudAccount")
		})
	}

	t.Run("host account is accepted", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		fake := &probeSTS{}
		h := hostCheckHandler(t, store, fake, nil)
		_, err := h.createAccount(ctx, adminRequest(roleARNBody(probeHostAcct, "")))
		require.NoError(t, err)
		store.AssertCalled(t, "CreateCloudAccount")
		assert.Equal(t, int32(1), fake.identityCalls.Load())
	})

	t.Run("role ARN set skips the host check", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		fake := &probeSTS{transportErr: true}
		h := hostCheckHandler(t, store, fake, nil)
		_, err := h.createAccount(ctx, adminRequest(roleARNBody(probeTenantAcct, probeTenantRole)))
		require.NoError(t, err)
		store.AssertCalled(t, "CreateCloudAccount")
		assert.Equal(t, int32(0), fake.identityCalls.Load())
	})

	t.Run("other auth modes skip the host check", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		fake := &probeSTS{transportErr: true}
		h := hostCheckHandler(t, store, fake, nil)
		body := `{"name":"Acme","provider":"aws","external_id":"` + probeTenantAcct + `","aws_auth_mode":"access_keys"}`
		_, err := h.createAccount(ctx, adminRequest(body))
		require.NoError(t, err)
		assert.Equal(t, int32(0), fake.identityCalls.Load())
	})
}

func TestUpdateAccount_EmptyRoleARNRequiresHostAccount(t *testing.T) {
	ctx := context.Background()
	for _, tc := range rejectedHostCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			h := hostCheckHandler(t, store, tc.fake, tc.cfgErr)
			result, err := h.updateAccount(ctx, adminRequest(roleARNBody(tc.externalID, "")), probeAccountID)
			assert.Nil(t, result)
			requireNotHost400(t, err, tc.wantMsg)
			store.AssertNotCalled(t, "UpdateCloudAccount")
		})
	}

	t.Run("host account is accepted", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		h := hostCheckHandler(t, store, &probeSTS{}, nil)
		_, err := h.updateAccount(ctx, adminRequest(roleARNBody(probeHostAcct, "")), probeAccountID)
		require.NoError(t, err)
		store.AssertCalled(t, "UpdateCloudAccount")
	})

	t.Run("out-of-scope account is not probed", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		store.GetCloudAccountFn = func(context.Context, string) (*config.CloudAccount, error) { return nil, nil }
		fake := &probeSTS{}
		h := hostCheckHandler(t, store, fake, nil)
		_, err := h.updateAccount(ctx, adminRequest(roleARNBody(probeHostAcct, "")), probeAccountID)
		require.ErrorIs(t, err, errNotFound)
		assert.Equal(t, int32(0), fake.identityCalls.Load())
	})
}

func TestApproveRegistration_EmptyRoleARNRequiresHostAccount(t *testing.T) {
	ctx := context.Background()
	for _, tc := range rejectedHostCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("GetAccountRegistration", mock.Anything, hostCheckRegID).
				Return(&config.AccountRegistration{ID: hostCheckRegID, Status: "pending", Provider: "aws", ExternalID: tc.externalID}, nil)
			// Registered so a regression fails on the assertions below
			// instead of panicking on an unexpected call.
			store.On("ApproveAccountRegistration", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(errors.New("registration must not be approved")).Maybe()
			h := hostCheckHandler(t, store, tc.fake, tc.cfgErr)
			// approveRegistration is called directly, without the router's
			// session check.
			relaxUnreachedStubs(&h.auth.(*MockAuthService).Mock, "ValidateSession")
			req := &events.LambdaFunctionURLRequest{Body: roleARNBody(tc.externalID, "")}
			result, err := h.approveRegistration(ctx, req, hostCheckRegID)
			assert.Nil(t, result)
			requireNotHost400(t, err, tc.wantMsg)
			store.AssertNotCalled(t, "ApproveAccountRegistration")
			store.AssertNotCalled(t, "TransitionRegistrationStatus")
			store.AssertNotCalled(t, "CreateCloudAccount")
		})
	}

	t.Run("host account is accepted", func(t *testing.T) {
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		store.On("GetAccountRegistration", mock.Anything, hostCheckRegID).
			Return(&config.AccountRegistration{ID: hostCheckRegID, Status: "pending", Provider: "aws", ExternalID: probeHostAcct}, nil)
		store.On("ApproveAccountRegistration", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		h := hostCheckHandler(t, store, &probeSTS{}, nil)
		relaxUnreachedStubs(&h.auth.(*MockAuthService).Mock, "ValidateSession")
		req := &events.LambdaFunctionURLRequest{Body: roleARNBody(probeHostAcct, "")}
		_, err := h.approveRegistration(ctx, req, hostCheckRegID)
		require.NoError(t, err)
		store.AssertCalled(t, "ApproveAccountRegistration", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}
