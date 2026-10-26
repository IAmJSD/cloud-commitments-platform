package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestBase32Decode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		hasError bool
	}{
		{
			name:     "valid base32",
			input:    "JBSWY3DPEHPK3PXP",
			hasError: false,
		},
		{
			name:     "empty string",
			input:    "",
			hasError: false,
		},
		{
			name:     "lowercase converted to uppercase",
			input:    "jbswy3dpehpk3pxp",
			hasError: false,
		},
		{
			name:     "with padding",
			input:    "GEZDGNBVGY3TQOJQ", // "12345678"
			hasError: false,
		},
		{
			name:     "invalid character",
			input:    "JBSWY3DPEHPK3PXP!",
			hasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := base32Decode(tt.input)
			if tt.hasError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				// Just verify it returns bytes without error
				assert.NotNil(t, result)
			}
		})
	}
}

func TestGenerateTOTP(t *testing.T) {
	// Test with a known secret and counter
	// Using RFC 6238 test vectors would be ideal but we just want coverage
	secret := "JBSWY3DPEHPK3PXP"

	tests := []struct {
		name    string
		counter int64
	}{
		{"counter 0", 0},
		{"counter 1", 1},
		{"counter 1000000", 1000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := generateTOTP(secret, tt.counter)
			// Should be a 6-digit code
			assert.Len(t, code, 6)
			// Should contain only digits
			for _, c := range code {
				assert.True(t, c >= '0' && c <= '9', "expected digit, got %c", c)
			}
		})
	}
}

func TestGenerateTOTP_InvalidSecret(t *testing.T) {
	// Invalid base32 secret should return empty string
	code := generateTOTP("INVALID!SECRET", 0)
	assert.Equal(t, "", code)
}

func TestVerifyTOTP(t *testing.T) {
	// Generate a code for the current time
	secret := "JBSWY3DPEHPK3PXP"
	currentTime := time.Now().Unix()
	timeStep := int64(30)
	counter := currentTime / timeStep

	// Generate the expected code
	expectedCode := generateTOTP(secret, counter)

	tests := []struct {
		name     string
		secret   string
		code     string
		expected bool
	}{
		{
			name:     "valid code for current time",
			secret:   secret,
			code:     expectedCode,
			expected: true,
		},
		{
			name:     "invalid code",
			secret:   secret,
			code:     "000000",
			expected: false,
		},
		{
			name:     "wrong secret",
			secret:   "DIFFERENTSECRETZ",
			code:     expectedCode,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := verifyTOTP(tt.secret, tt.code)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestVerifyTOTP_TimeWindow(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	currentTime := time.Now().Unix()
	timeStep := int64(30)

	// Test that codes from adjacent time windows are accepted
	for _, offset := range []int64{-1, 0, 1} {
		counter := (currentTime / timeStep) + offset
		code := generateTOTP(secret, counter)

		result := verifyTOTP(secret, code)
		assert.True(t, result, "code from time window offset %d should be valid", offset)
	}
}

// ---------------------------------------------------------------
// Tests for the MFA enrollment / disable / regenerate lifecycle
// added in issue #497.
// ---------------------------------------------------------------

func TestGenerateMFASecret(t *testing.T) {
	a, err := generateMFASecret()
	require.NoError(t, err)
	b, err := generateMFASecret()
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "two successive secrets must differ")
	// Unpadded base32 of 20 bytes = ceil(20 * 8 / 5) = 32 chars.
	assert.Len(t, a, 32)
}

func TestBuildProvisioningURI(t *testing.T) {
	uri := buildProvisioningURI("user@example.com", "JBSWY3DPEHPK3PXP")
	assert.True(t, strings.HasPrefix(uri, "otpauth://totp/"))
	assert.Contains(t, uri, "secret=JBSWY3DPEHPK3PXP")
	assert.Contains(t, uri, "issuer=CUDly")
	assert.Contains(t, uri, "algorithm=SHA1")
	assert.Contains(t, uri, "digits=6")
	assert.Contains(t, uri, "period=30")
}

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := generateRecoveryCodes(10)
	require.NoError(t, err)
	assert.Len(t, codes, 10)
	seen := map[string]struct{}{}
	for _, c := range codes {
		// Format: XXXX-XXXX (8 chars + dash)
		assert.Len(t, c, recoveryCodeLength+1)
		assert.Equal(t, byte('-'), c[recoveryCodeLength/2])
		assert.NotContains(t, seen, c, "recovery code collision is astronomically unlikely")
		seen[c] = struct{}{}
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	assert.Equal(t, "ABCD2345", normalizeRecoveryCode("abcd-2345"))
	assert.Equal(t, "ABCD2345", normalizeRecoveryCode(" abcd 2345 "))
	assert.Equal(t, "ABCD2345", normalizeRecoveryCode("ABCD-2345"))
}

// totpFor returns a valid TOTP code for the given secret at the
// current time step. Helper for the service tests below.
func totpFor(secret string) string {
	timeStep := int64(30)
	return generateTOTP(secret, time.Now().Unix()/timeStep)
}

func TestMFASetup_HappyPath(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = false
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	var pending string
	mockStore.On("SetPendingMFASecret", ctx, user.ID, mock.AnythingOfType("string"), mock.AnythingOfType("time.Time")).Run(func(args mock.Arguments) {
		pending = args.String(2)
		assert.WithinDuration(t, time.Now().Add(mfaPendingExpiry), args.Get(3).(time.Time), time.Minute)
	}).Return(nil).Once()

	res, err := service.MFASetup(ctx, user.ID, "SecurePass@123")
	require.NoError(t, err)
	assert.Equal(t, pending, res.Secret, "the returned secret must be the persisted pending one")
	assert.True(t, strings.HasPrefix(res.ProvisioningURI, "otpauth://totp/"))
	mockStore.AssertExpectations(t)
}

func TestMFASetup_WrongPassword(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFASetup(ctx, user.ID, "WrongPassword!@#")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid password")
	mockStore.AssertNotCalled(t, "SetPendingMFASecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestMFAEnable_HappyPath(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(mfaPendingExpiry)
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("EnableMFA", ctx, user.ID, secret, currentTOTPStep(), mock.AnythingOfType("[]string")).Run(func(args mock.Arguments) {
		assert.Len(t, args.Get(4).([]string), recoveryCodeCount)
	}).Return(nil).Once()

	codes, err := service.MFAEnable(ctx, user.ID, totpFor(secret))
	require.NoError(t, err)
	assert.Len(t, codes, recoveryCodeCount)
	mockStore.AssertExpectations(t)
}

func TestMFAEnable_ExpiredPending(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(-1 * time.Minute) // already expired
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFAEnable(ctx, user.ID, totpFor(secret))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
	mockStore.AssertExpectations(t) // an expired enrollment is rejected without a write
}

func TestMFAEnable_NoPending(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFAEnable(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no MFA enrollment in progress")
}

func TestMFAEnable_WrongCode(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(mfaPendingExpiry)
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := service.MFAEnable(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid MFA code")
}

func TestMFADisable_WithTOTP(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	user.MFARecoveryCodes = []string{"$2a$04$hashedstub"} // doesn't matter, won't be tested

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(true, nil).Once()
	mockStore.On("DisableMFA", ctx, user.ID, secret, []string{"$2a$04$hashedstub"}).Return(nil).Once()

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", totpFor(secret))
	require.NoError(t, err)
	mockStore.AssertExpectations(t)
}

func TestMFADisable_WithRecoveryCode(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret

	plaintextCode := "ABCD-2345"
	hash, err := service.hashRecoveryCode(plaintextCode)
	require.NoError(t, err)
	user.MFARecoveryCodes = []string{hash}

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("DisableMFA", ctx, user.ID, secret, []string{hash}).Return(nil).Once()

	err = service.MFADisable(ctx, user.ID, "SecurePass@123", plaintextCode)
	require.NoError(t, err)
}

// Issue #568 (c): spending a recovery code that is not the last one shifts the
// user's slice in place, so DisableMFA must still be guarded by the codes as
// read, not by the shifted slice.
func TestMFADisable_WithRecoveryCodePassesCodesAsRead(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	codes := make([]string, 0, 3)
	for _, plaintext := range []string{"ABCD-2345", "EFGH-6789", "JKLM-2345"} {
		hash, err := service.hashRecoveryCode(plaintext)
		require.NoError(t, err)
		codes = append(codes, hash)
	}
	user.MFARecoveryCodes = codes
	asRead := slices.Clone(codes)

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("DisableMFA", ctx, user.ID, user.MFASecret, asRead).Return(nil).Once()

	require.NoError(t, service.MFADisable(ctx, user.ID, "SecurePass@123", "ABCD-2345"))
	mockStore.AssertExpectations(t)
}

func TestMFADisable_WrongPassword(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	err := service.MFADisable(ctx, user.ID, "wrong", "000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid password")
	mockStore.AssertNotCalled(t, "DisableMFA", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestMFADisable_IdempotentWhenAlreadyDisabled(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = false
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClearPendingMFASecret", ctx, user.ID).Return(nil).Once()

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", "")
	require.NoError(t, err)
	mockStore.AssertExpectations(t)
}

func TestMFASetupAndEnable_RefusedWhileEnabled(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	pending := "GEZDGNBVGY3TQOJQ"
	expiresAt := time.Now().Add(mfaPendingExpiry)
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	user.MFAPendingSecret = pending
	user.MFAPendingSecretExpiresAt = &expiresAt
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	res, err := service.MFASetup(ctx, user.ID, "SecurePass@123")
	require.ErrorIs(t, err, ErrMFAAlreadyEnabled)
	assert.Nil(t, res)
	codes, err := service.MFAEnable(ctx, user.ID, totpFor(pending))
	require.ErrorIs(t, err, ErrMFAAlreadyEnabled)
	assert.Nil(t, codes)
	mockStore.AssertExpectations(t) // no write reached the store
}

func TestMFASetup_WrongPasswordWhileEnabled_ReportsPassword(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFASetup(ctx, user.ID, "WrongPassword!@#")
	require.ErrorIs(t, err, ErrMFAInvalidPassword)
}

func TestMFARegenerateRecoveryCodes_HappyPath(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	// pre-existing hash that should NOT survive regeneration
	user.MFARecoveryCodes = []string{"$2a$04$preexistingstub"}

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(true, nil).Once()
	var captured []string
	mockStore.On("ReplaceMFARecoveryCodes", ctx, user.ID, secret, []string{"$2a$04$preexistingstub"}, mock.Anything).Run(func(args mock.Arguments) {
		captured = append([]string{}, args.Get(4).([]string)...)
	}).Return(nil).Once()

	codes, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, totpFor(secret))
	require.NoError(t, err)
	assert.Len(t, codes, recoveryCodeCount)
	assert.Len(t, captured, recoveryCodeCount, "regenerate must replace the entire slice")
	// The pre-existing stub must NOT be present.
	for _, h := range captured {
		assert.NotEqual(t, "$2a$04$preexistingstub", h)
	}
}

func TestMFARegenerateRecoveryCodes_WrongTOTP(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid MFA code")
}

func TestLogin_WithMFA_RecoveryCode_ConsumedOnce(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	plaintextCode := "ABCD-2345"
	hash, err := service.hashRecoveryCode(plaintextCode)
	require.NoError(t, err)

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	user.MFARecoveryCodes = []string{hash}

	mockStore.On("GetUserByEmail", ctx, user.Email).Return(user, nil)
	mockStore.On("ConsumeMFARecoveryCode", ctx, user.ID, []string{hash}, []string{}).Return(nil).Once()
	mockStore.On("RecordSuccessfulLogin", ctx, user.ID).Return(nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil)
	mockStore.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil)

	resp, err := service.Login(ctx, LoginRequest{
		Email:    user.Email,
		Password: "SecurePass@123",
		MFACode:  plaintextCode,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Empty(t, user.MFARecoveryCodes, "consumed recovery code must be removed from the slice")
}

func TestLogin_WithMFA_RecoveryCode_PersistenceFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := new(MockStore)
	service := createTestService(store, new(MockEmailSender))
	code := "ABCD-2345"
	hash, err := service.hashRecoveryCode(code)
	require.NoError(t, err)
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	user.MFARecoveryCodes = []string{hash}

	for range 2 {
		snapshot := *user
		snapshot.MFARecoveryCodes = append([]string(nil), user.MFARecoveryCodes...)
		store.On("GetUserByEmail", ctx, user.Email).Return(&snapshot, nil).Once()
	}
	store.On("ConsumeMFARecoveryCode", ctx, user.ID, []string{hash}, []string{}).
		Return(errors.New("consumption write failed")).Twice()

	for range 2 {
		response, loginErr := service.Login(ctx, LoginRequest{
			Email: user.Email, Password: "SecurePass@123", MFACode: code,
		})
		assert.ErrorIs(t, loginErr, ErrInvalidMFACode)
		assert.True(t, response == nil, "failed consumption must not return a token")
	}
	store.AssertNotCalled(t, "CreateSession", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "RecordSuccessfulLogin", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "RecordFailedLogin", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// ---------------------------------------------------------------
// Sentinel-identity tests (issue #512).
//
// Each test asserts that the real service returns the expected typed
// sentinel so that a future rename of the error message string in
// service_mfa.go causes a compile-time or test failure rather than a
// silent HTTP-status drift. errors.Is is the contract; the message
// text is NOT the contract (but is preserved for human readability).
// ---------------------------------------------------------------

func TestMFASetup_WrongPassword_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFASetup(ctx, user.ID, "WrongPassword!@#")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAInvalidPassword),
		"MFASetup wrong-password must return ErrMFAInvalidPassword, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFAEnable_NoPending_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFAEnable(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFANoEnrollmentInProgress),
		"MFAEnable with no pending enrollment must return ErrMFANoEnrollmentInProgress, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFAEnable_ExpiredPending_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(-1 * time.Minute)
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFAEnable(ctx, user.ID, totpFor(secret))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAEnrollmentExpired),
		"MFAEnable with expired enrollment must return ErrMFAEnrollmentExpired, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFAEnable_WrongCode_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(mfaPendingExpiry)
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := service.MFAEnable(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAInvalidCode),
		"MFAEnable wrong code must return ErrMFAInvalidCode, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFADisable_WrongPassword_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	err := service.MFADisable(ctx, user.ID, "wrong", "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAInvalidPassword),
		"MFADisable wrong password must return ErrMFAInvalidPassword, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFADisable_NoCode_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFACodeRequired),
		"MFADisable empty code must return ErrMFACodeRequired, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFADisable_WrongCode_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	user.MFARecoveryCodes = []string{"$2a$04$hashedstub"} // won't match any real code
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAInvalidCode),
		"MFADisable wrong code must return ErrMFAInvalidCode, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFARegenerateRecoveryCodes_NotEnabled_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = false
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)

	_, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFANotEnabled),
		"MFARegenerateRecoveryCodes when disabled must return ErrMFANotEnabled, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMFARegenerateRecoveryCodes_WrongCode_ReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = "JBSWY3DPEHPK3PXP"
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, "000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMFAInvalidCode),
		"MFARegenerateRecoveryCodes wrong TOTP must return ErrMFAInvalidCode, got: %v", err)
	mockStore.AssertExpectations(t)
}

func TestMatchTOTP_ReturnsMatchedCounter(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	current := time.Now().Unix() / 30
	for _, offset := range []int64{-1, 0, 1} {
		counter, ok := matchTOTP(secret, generateTOTP(secret, current+offset))
		assert.True(t, ok)
		assert.Equal(t, current+offset, counter)
	}
	_, ok := matchTOTP(secret, generateTOTP(secret, current+5))
	assert.False(t, ok)
}

func TestMFADisable_ReplayedTOTPIsRejected(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(false, nil).Once()
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", totpFor(secret))
	require.ErrorIs(t, err, ErrMFAInvalidCode)
	mockStore.AssertNotCalled(t, "DisableMFA", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

func TestMFARegenerateRecoveryCodes_ReplayedTOTPIsRejected(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(false, nil).Once()
	mockStore.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, totpFor(secret))
	require.ErrorIs(t, err, ErrMFAInvalidCode)
	mockStore.AssertNotCalled(t, "ReplaceMFARecoveryCodes", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestMFARegenerateRecoveryCodes_ClaimFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(false, errors.New("db down")).Once()

	_, err := service.MFARegenerateRecoveryCodes(ctx, user.ID, totpFor(secret))
	require.ErrorContains(t, err, "db down")
	mockStore.AssertNotCalled(t, "ReplaceMFARecoveryCodes", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestMFADisable_ClaimFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	user := createTestUser(t, "SecurePass@123")
	user.MFAEnabled = true
	user.MFASecret = secret
	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(false, errors.New("db down")).Once()

	err := service.MFADisable(ctx, user.ID, "SecurePass@123", totpFor(secret))
	require.ErrorContains(t, err, "db down")
	mockStore.AssertNotCalled(t, "DisableMFA", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Issue #442: a locked account is refused before any code is checked, with
// the opaque error and without spending a TOTP counter or writing state.
func TestMFALifecycle_LockedAccountRefusedBeforeCodeCheck(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	calls := map[string]func(s *Service, user *User) error{
		"enable": func(s *Service, user *User) error {
			_, err := s.MFAEnable(context.Background(), user.ID, totpFor(secret))
			return err
		},
		"disable": func(s *Service, user *User) error {
			return s.MFADisable(context.Background(), user.ID, "SecurePass@123", totpFor(secret))
		},
		"regenerate": func(s *Service, user *User) error {
			_, err := s.MFARegenerateRecoveryCodes(context.Background(), user.ID, totpFor(secret))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			mockStore := new(MockStore)
			service := createTestService(mockStore, new(MockEmailSender))
			until := time.Now().Add(time.Minute)
			expires := time.Now().Add(mfaPendingExpiry)
			user := createTestUser(t, "SecurePass@123")
			user.MFAEnabled, user.MFASecret, user.LockedUntil = true, secret, &until
			user.MFAPendingSecret, user.MFAPendingSecretExpiresAt = secret, &expires
			mockStore.On("GetUserByID", mock.Anything, user.ID).Return(user, nil)
			// Permissive stubs so an unfixed service fails by assertion, not by panic.
			writes := []string{"ClaimTOTPCounter", "EnableMFA", "DisableMFA", "ReplaceMFARecoveryCodes", "ClearPendingMFASecret", "RecordFailedLogin"}
			mockStore.On("ClaimTOTPCounter", mock.Anything, user.ID, mock.Anything).Return(true, nil).Maybe()
			mockStore.On("EnableMFA", mock.Anything, user.ID, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			mockStore.On("DisableMFA", mock.Anything, user.ID, mock.Anything, mock.Anything).Return(nil).Maybe()
			mockStore.On("ReplaceMFARecoveryCodes", mock.Anything, user.ID, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			mockStore.On("ClearPendingMFASecret", mock.Anything, user.ID).Return(nil).Maybe()
			mockStore.On("RecordFailedLogin", mock.Anything, user.ID).Return(nil).Maybe()

			require.ErrorIs(t, call(service, user), ErrMFAAuthFailed)
			for _, method := range writes {
				for _, c := range mockStore.Calls {
					assert.NotEqual(t, method, c.Method, "locked account must not reach %s", method)
				}
			}
		})
	}
}
