package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// MockStore is a mock implementation of the auth store for testing.
type MockStore struct {
	mock.Mock
}

func (m *MockStore) GetUserByID(ctx context.Context, userID string) (*User, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	u, ok := args.Get(0).(*User)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetUserByID: expected *User, got %T", args.Get(0)))
	}
	return u, args.Error(1)
}

func (m *MockStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	args := m.Called(ctx, email)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	u, ok := args.Get(0).(*User)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetUserByEmail: expected *User, got %T", args.Get(0)))
	}
	return u, args.Error(1)
}

func (m *MockStore) CreateUser(ctx context.Context, user *User) error {
	args := m.Called(ctx, user)
	return args.Error(0)
}

func (m *MockStore) UpdateUserAdminFields(ctx context.Context, user *User, readEmail string, readGroupIDs []string, readActive bool) error {
	return m.Called(ctx, user, readEmail, readGroupIDs, readActive).Error(0)
}

func (m *MockStore) UpdateUserCredentials(ctx context.Context, user *User, readEmail, readPasswordHash string) error {
	return m.Called(ctx, user, readEmail, readPasswordHash).Error(0)
}

func (m *MockStore) SetPasswordResetToken(ctx context.Context, user *User, readExpiry *time.Time) error {
	return m.Called(ctx, user, readExpiry).Error(0)
}

func (m *MockStore) CompletePasswordReset(ctx context.Context, user *User, readResetToken, readPasswordHash string) error {
	return m.Called(ctx, user, readResetToken, readPasswordHash).Error(0)
}

func (m *MockStore) ConsumePasswordResetToken(ctx context.Context, userID, readResetToken string) error {
	return m.Called(ctx, userID, readResetToken).Error(0)
}

func (m *MockStore) ConsumeMFARecoveryCode(ctx context.Context, userID string, readCodes, remaining []string) error {
	return m.Called(ctx, userID, readCodes, remaining).Error(0)
}

func (m *MockStore) SetPendingMFASecret(ctx context.Context, userID, secret string, expiresAt time.Time) error {
	return m.Called(ctx, userID, secret, expiresAt).Error(0)
}

func (m *MockStore) EnableMFA(ctx context.Context, userID, pendingSecret string, totpCounter int64, recoveryHashes []string) error {
	return m.Called(ctx, userID, pendingSecret, totpCounter, recoveryHashes).Error(0)
}

func (m *MockStore) ReplaceMFARecoveryCodes(ctx context.Context, userID, readSecret string, readCodes, newHashes []string) error {
	return m.Called(ctx, userID, readSecret, readCodes, newHashes).Error(0)
}

func (m *MockStore) DisableMFA(ctx context.Context, userID, readSecret string, readCodes []string) error {
	return m.Called(ctx, userID, readSecret, readCodes).Error(0)
}

func (m *MockStore) ClearPendingMFASecret(ctx context.Context, userID string) error {
	return m.Called(ctx, userID).Error(0)
}

func (m *MockStore) ClaimTOTPCounter(ctx context.Context, userID string, counter int64) (bool, error) {
	args := m.Called(ctx, userID, counter)
	return args.Bool(0), args.Error(1)
}

func (m *MockStore) CreateMFALoginToken(ctx context.Context, tokenHash, userID string, passwordVersion int64, ttl time.Duration) (time.Time, error) {
	args := m.Called(ctx, tokenHash, userID, passwordVersion, ttl)
	expiresAt, ok := args.Get(0).(time.Time)
	if !ok {
		panic(fmt.Sprintf("MockStore.CreateMFALoginToken: expected time.Time, got %T", args.Get(0)))
	}
	return expiresAt, args.Error(1)
}

func (m *MockStore) ClaimMFALoginTokenAttempt(ctx context.Context, tokenHash string, maxAttempts int) (*MFALoginToken, error) {
	args := m.Called(ctx, tokenHash, maxAttempts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	tok, ok := args.Get(0).(*MFALoginToken)
	if !ok {
		panic(fmt.Sprintf("MockStore.ClaimMFALoginTokenAttempt: expected *MFALoginToken, got %T", args.Get(0)))
	}
	return tok, args.Error(1)
}

func (m *MockStore) ConsumeMFALoginToken(ctx context.Context, tokenHash string) (bool, error) {
	args := m.Called(ctx, tokenHash)
	return args.Bool(0), args.Error(1)
}

func (m *MockStore) RecordFailedLogin(ctx context.Context, userID string) error {
	return m.Called(ctx, userID).Error(0)
}

func (m *MockStore) RecordSuccessfulLogin(ctx context.Context, userID string) error {
	return m.Called(ctx, userID).Error(0)
}

func (m *MockStore) DeleteUser(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *MockStore) ListUsers(ctx context.Context) ([]User, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	u, ok := args.Get(0).([]User)
	if !ok {
		panic(fmt.Sprintf("MockStore.ListUsers: expected []User, got %T", args.Get(0)))
	}
	return u, args.Error(1)
}

func (m *MockStore) GetUserByResetToken(ctx context.Context, token string) (*User, error) {
	args := m.Called(ctx, token)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	u, ok := args.Get(0).(*User)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetUserByResetToken: expected *User, got %T", args.Get(0)))
	}
	return u, args.Error(1)
}

func (m *MockStore) AdminExists(ctx context.Context) (bool, error) {
	args := m.Called(ctx)
	return args.Bool(0), args.Error(1)
}

func (m *MockStore) CreateAdminIfNone(ctx context.Context, user *User) (bool, error) {
	args := m.Called(ctx, user)
	return args.Bool(0), args.Error(1)
}

func (m *MockStore) GetGroup(ctx context.Context, groupID string) (*Group, error) {
	args := m.Called(ctx, groupID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	g, ok := args.Get(0).(*Group)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetGroup: expected *Group, got %T", args.Get(0)))
	}
	return g, args.Error(1)
}

func (m *MockStore) CreateGroup(ctx context.Context, group *Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *MockStore) UpdateGroup(ctx context.Context, group *Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *MockStore) DeleteGroup(ctx context.Context, groupID string) error {
	args := m.Called(ctx, groupID)
	return args.Error(0)
}

func (m *MockStore) ListGroups(ctx context.Context) ([]Group, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	g, ok := args.Get(0).([]Group)
	if !ok {
		panic(fmt.Sprintf("MockStore.ListGroups: expected []Group, got %T", args.Get(0)))
	}
	return g, args.Error(1)
}

func (m *MockStore) CountGroupMembers(ctx context.Context, groupID string) (int, error) {
	args := m.Called(ctx, groupID)
	return args.Int(0), args.Error(1)
}

func (m *MockStore) CreateSession(ctx context.Context, session *Session) error {
	args := m.Called(ctx, session)
	return args.Error(0)
}

func (m *MockStore) GetSession(ctx context.Context, token string) (*Session, error) {
	args := m.Called(ctx, token)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	s, ok := args.Get(0).(*Session)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetSession: expected *Session, got %T", args.Get(0)))
	}
	return s, args.Error(1)
}

func (m *MockStore) DeleteSession(ctx context.Context, token string) error {
	args := m.Called(ctx, token)
	return args.Error(0)
}

func (m *MockStore) DeleteUserSessions(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *MockStore) CleanupExpiredSessions(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// API Key operations.
func (m *MockStore) CreateAPIKey(ctx context.Context, key *UserAPIKey) error {
	args := m.Called(ctx, key)
	return args.Error(0)
}

func (m *MockStore) GetAPIKeyByID(ctx context.Context, keyID string) (*UserAPIKey, error) {
	args := m.Called(ctx, keyID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	k, ok := args.Get(0).(*UserAPIKey)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetAPIKeyByID: expected *UserAPIKey, got %T", args.Get(0)))
	}
	return k, args.Error(1)
}

func (m *MockStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*UserAPIKey, error) {
	args := m.Called(ctx, keyHash)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	k, ok := args.Get(0).(*UserAPIKey)
	if !ok {
		panic(fmt.Sprintf("MockStore.GetAPIKeyByHash: expected *UserAPIKey, got %T", args.Get(0)))
	}
	return k, args.Error(1)
}

func (m *MockStore) ListAPIKeysByUser(ctx context.Context, userID string) ([]*UserAPIKey, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	k, ok := args.Get(0).([]*UserAPIKey)
	if !ok {
		panic(fmt.Sprintf("MockStore.ListAPIKeysByUser: expected []*UserAPIKey, got %T", args.Get(0)))
	}
	return k, args.Error(1)
}

func (m *MockStore) UpdateAPIKey(ctx context.Context, key *UserAPIKey) error {
	args := m.Called(ctx, key)
	return args.Error(0)
}

func (m *MockStore) RecordAPIKeyUsage(ctx context.Context, keyID string, delta int64) error {
	args := m.Called(ctx, keyID, delta)
	return args.Error(0)
}

func (m *MockStore) DeleteAPIKey(ctx context.Context, keyID string) error {
	args := m.Called(ctx, keyID)
	return args.Error(0)
}

func (m *MockStore) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// MockEmailSender is a mock implementation of the email sender for testing.
type MockEmailSender struct {
	mock.Mock
}

func (m *MockEmailSender) SendPasswordResetEmail(ctx context.Context, email, resetURL string) error {
	args := m.Called(ctx, email, resetURL)
	return args.Error(0)
}

func (m *MockEmailSender) SendUserInviteEmail(ctx context.Context, email, setupURL string) error {
	args := m.Called(ctx, email, setupURL)
	return args.Error(0)
}

// Verify that MockStore implements StoreInterface.
var _ StoreInterface = (*MockStore)(nil)

// Verify that MockEmailSender implements EmailSenderInterface.
var _ EmailSenderInterface = (*MockEmailSender)(nil)

// testCSRFKey is a fixed 32-byte key used across all test services so that
// tests can reproduce the CSRF token derived by deriveCSRFToken without
// calling a real key-generation path.
var testCSRFKey = []byte("test-csrf-key-32-bytes-padded---")

// TestCSRFKey returns the fixed 32-byte CSRF key shared by test services.
// Pass it as ServiceConfig.CSRFKey so a service built via NewService derives
// CSRF tokens deterministically (no ephemeral random key), letting tests in
// other packages reproduce the expected token with DeriveTestCSRFToken.
func TestCSRFKey() []byte {
	// Return a copy so callers cannot mutate the package-level key.
	key := make([]byte, len(testCSRFKey))
	copy(key, testCSRFKey)
	return key
}

// TestPasswordHash returns a bcrypt hash of password at bcrypt.MinCost, for
// test fixtures in this and other packages that need a User.PasswordHash
// verifyPassword will accept (issue #102: CreateAPIKey and the password-
// rotation paths now re-verify the caller's password). MinCost keeps the
// hash cheap to compute since tests only need a hash that round-trips
// through bcrypt.CompareHashAndPassword, not the production work factor.
func TestPasswordHash(t testing.TB, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return string(hash)
}

// DeriveTestCSRFToken returns the CSRF token a service configured with
// TestCSRFKey expects for the given raw session token. It reuses the
// production derivation (HMAC-SHA256(key, rawSessionToken)) so tests assert
// the real contract rather than duplicating the crypto.
func DeriveTestCSRFToken(rawSessionToken string) string {
	return deriveCSRFToken(testCSRFKey, rawSessionToken)
}

// newTestService returns a minimal Service with fast bcrypt for unit tests.
// It has no store or email sender — use createTestService() for tests that need mocks.
func newTestService() *Service {
	return &Service{
		bcryptCostOverride: bcrypt.MinCost,
		csrfKey:            testCSRFKey,
	}
}

// createTestService creates a service with mocks for testing.
func createTestService(mockStore *MockStore, mockEmail *MockEmailSender) *Service {
	return &Service{
		store:              mockStore,
		emailSender:        mockEmail,
		sessionDuration:    24 * time.Hour,
		dashboardURL:       "https://dashboard.example.com",
		bcryptCostOverride: bcrypt.MinCost,
		csrfKey:            testCSRFKey,
	}
}

// createTestUser creates a user with hashed password for testing.
func createTestUser(t *testing.T, password string) *User {
	t.Helper()

	s := newTestService()
	hash, err := s.hashPassword(password)
	require.NoError(t, err)

	return &User{
		ID:           "user-123",
		Email:        "test@example.com",
		PasswordHash: hash,
		GroupIDs:     []string{DefaultAdminGroupID},
		Active:       true,
		CreatedAt:    time.Now(),
	}
}
