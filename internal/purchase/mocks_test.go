package purchase

import (
	"context"
	"slices"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/mock"
)

// MockConfigStore is the shared testify mock for config.StoreInterface.
// All Fn-override fields (GetPlanAccountsFn, SavePurchaseExecutionFn, etc.)
// and default behaviors live in internal/mocks.
type MockConfigStore = mocks.MockConfigStore

// expectationAsserter is any testify mock (*mock.Mock or a type embedding it).
type expectationAsserter interface {
	AssertExpectations(t mock.TestingT) bool
}

// assertMocksOnCleanup makes t fail at cleanup when any required (non-.Maybe())
// expectation registered on ms was never consumed, so an .On(...) a test sets up
// is also a check that the call happened (issue #35). Call it right after
// constructing the mocks; registering it before the expectations is fine.
func assertMocksOnCleanup(t *testing.T, ms ...expectationAsserter) {
	t.Helper()
	t.Cleanup(func() {
		for _, m := range ms {
			m.AssertExpectations(t)
		}
	})
}

// optionalCalls marks the expectations a shared helper registered for methods
// as optional, for a test whose path never makes those calls. Times(0) clears
// a .Once() count too: testify reports an unconsumed .Once() even when it is
// .Maybe().
func optionalCalls(m *mock.Mock, methods ...string) {
	for _, c := range m.ExpectedCalls {
		if slices.Contains(methods, c.Method) {
			c.Maybe().Times(0)
		}
	}
}

// MockProviderFactory is a mock implementation of ProviderFactoryInterface.
type MockProviderFactory struct {
	mock.Mock
}

func (m *MockProviderFactory) CreateAndValidateProvider(ctx context.Context, name string, cfg *provider.Config) (provider.Provider, error) {
	args := m.Called(ctx, name, cfg)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(provider.Provider), args.Error(1)
}

// MockProvider is a mock implementation of provider.Provider.
type MockProvider struct {
	mock.Mock
}

func (m *MockProvider) Name() string {
	return "mock"
}

func (m *MockProvider) DisplayName() string {
	return "Mock Provider"
}

func (m *MockProvider) IsConfigured() bool {
	return true
}

func (m *MockProvider) GetCredentials() (provider.Credentials, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(provider.Credentials), args.Error(1)
}

func (m *MockProvider) ValidateCredentials(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockProvider) GetAccounts(ctx context.Context) ([]common.Account, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]common.Account), args.Error(1)
}

func (m *MockProvider) GetRegions(ctx context.Context) ([]common.Region, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]common.Region), args.Error(1)
}

func (m *MockProvider) GetDefaultRegion() string {
	return "us-east-1"
}

func (m *MockProvider) GetSupportedServices() []common.ServiceType {
	return []common.ServiceType{common.ServiceEC2, common.ServiceRDS}
}

func (m *MockProvider) GetServiceClient(ctx context.Context, serviceType common.ServiceType, region string) (provider.ServiceClient, error) {
	args := m.Called(ctx, serviceType, region)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(provider.ServiceClient), args.Error(1)
}

func (m *MockProvider) GetRecommendationsClient(ctx context.Context) (provider.RecommendationsClient, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(provider.RecommendationsClient), args.Error(1)
}

// MockServiceClient is a mock implementation of provider.ServiceClient.
type MockServiceClient struct {
	mock.Mock
}

func (m *MockServiceClient) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	args := m.Called(ctx, rec, opts)
	return args.Get(0).(common.PurchaseResult), args.Error(1)
}

func (m *MockServiceClient) GetRecommendations(ctx context.Context, params *common.RecommendationParams) ([]common.Recommendation, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]common.Recommendation), args.Error(1)
}

func (m *MockServiceClient) GetServiceType() common.ServiceType {
	args := m.Called()
	return args.Get(0).(common.ServiceType)
}

func (m *MockServiceClient) GetRegion() string {
	args := m.Called()
	return args.String(0)
}

func (m *MockServiceClient) GetExistingCommitments(ctx context.Context) ([]common.Commitment, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]common.Commitment), args.Error(1)
}

func (m *MockServiceClient) ValidateOffering(ctx context.Context, rec common.Recommendation) error {
	args := m.Called(ctx, rec)
	return args.Error(0)
}

func (m *MockServiceClient) GetOfferingDetails(ctx context.Context, rec common.Recommendation) (*common.OfferingDetails, error) {
	args := m.Called(ctx, rec)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*common.OfferingDetails), args.Error(1)
}

func (m *MockServiceClient) GetValidResourceTypes(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]string), args.Error(1)
}

// MockEmailSender is a mock implementation of email.SenderInterface.
type MockEmailSender struct {
	mock.Mock
}

func (m *MockEmailSender) SendNotification(ctx context.Context, subject, message string) error {
	args := m.Called(ctx, subject, message)
	return args.Error(0)
}

func (m *MockEmailSender) SendToEmail(ctx context.Context, toEmail, subject, body string) error {
	args := m.Called(ctx, toEmail, subject, body)
	return args.Error(0)
}

func (m *MockEmailSender) SendToEmailWithCCMultipart(_ context.Context, _ string, _ []string, _, _, _ string) error {
	return nil
}

func (m *MockEmailSender) SendNewRecommendationsNotification(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

func (m *MockEmailSender) SendScheduledPurchaseNotification(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

func (m *MockEmailSender) SendPurchaseConfirmation(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

func (m *MockEmailSender) SendPurchaseFailedNotification(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

// allowFailedNotification lets a test whose execution ends failed or
// partially_completed receive the purchase-failed email that outcome now sends
// (issue #714) without asserting on it. Tests that pin the notification itself
// register their own expectation instead (failure_notify_test.go).
func allowFailedNotification(s *MockEmailSender) *MockEmailSender {
	s.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Maybe()
	return s
}

func (m *MockEmailSender) SendPasswordResetEmail(ctx context.Context, emailAddr, resetURL string) error {
	args := m.Called(ctx, emailAddr, resetURL)
	return args.Error(0)
}

func (m *MockEmailSender) SendUserInviteEmail(ctx context.Context, emailAddr, setupURL string) error {
	args := m.Called(ctx, emailAddr, setupURL)
	return args.Error(0)
}

func (m *MockEmailSender) SendRIExchangePendingApproval(ctx context.Context, data email.RIExchangeNotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

func (m *MockEmailSender) SendRIExchangeCompleted(ctx context.Context, data email.RIExchangeNotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}

func (m *MockEmailSender) SendPurchaseApprovalRequest(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}
func (m *MockEmailSender) SendPurchaseScheduledNotification(_ context.Context, _ email.NotificationData) error {
	return nil
}
func (m *MockEmailSender) SendPurchaseExecutedNotification(ctx context.Context, data email.NotificationData) error {
	args := m.Called(ctx, data)
	return args.Error(0)
}
func (m *MockEmailSender) SendRegistrationReceivedNotification(_ context.Context, _ email.RegistrationNotificationData) error {
	return nil
}
func (m *MockEmailSender) SendRegistrationDecisionNotification(_ context.Context, _ string, _ email.RegistrationDecisionData) error {
	return nil
}

// Verify MockEmailSender implements email.SenderInterface.
var _ email.SenderInterface = (*MockEmailSender)(nil)

// MockSTSClient is a mock implementation of STSClient.
type MockSTSClient struct {
	mock.Mock
}

func (m *MockSTSClient) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*sts.GetCallerIdentityOutput), args.Error(1)
}

// Verify MockSTSClient implements STSClient.
var _ STSClient = (*MockSTSClient)(nil)

// MockCredentialStore is a stub credentials.CredentialStore used in tests.
// All methods are no-ops; individual tests may override behavior via fields.
type MockCredentialStore struct {
	LoadRawFn func(ctx context.Context, accountID, credType string) ([]byte, error)
}

var _ credentials.CredentialStore = (*MockCredentialStore)(nil)

func (m *MockCredentialStore) SaveCredential(_ context.Context, _, _ string, _ []byte) error {
	return nil
}

func (m *MockCredentialStore) DeleteCredential(_ context.Context, _, _ string) error {
	return nil
}

func (m *MockCredentialStore) HasCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *MockCredentialStore) LoadRaw(ctx context.Context, accountID, credType string) ([]byte, error) {
	if m.LoadRawFn != nil {
		return m.LoadRawFn(ctx, accountID, credType)
	}
	return nil, nil
}

func (m *MockCredentialStore) EncryptPayload(plaintext []byte) (string, error) {
	return string(plaintext), nil // no-op: return plaintext as "encrypted" for tests
}

func (m *MockCredentialStore) DecryptPayload(ciphertext string) ([]byte, error) {
	return []byte(ciphertext), nil // no-op: return ciphertext as "decrypted" for tests
}

// MockAssumeRoleSTS is a stub credentials.STSClient (AssumeRole only) used in tests.
type MockAssumeRoleSTS struct {
	mock.Mock
}

var _ credentials.STSClient = (*MockAssumeRoleSTS)(nil)

func (m *MockAssumeRoleSTS) AssumeRole(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*sts.AssumeRoleOutput), args.Error(1)
}

func (m *MockAssumeRoleSTS) AssumeRoleWithWebIdentity(ctx context.Context, params *sts.AssumeRoleWithWebIdentityInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, nil
}
