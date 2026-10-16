package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// currentTOTPStep matches the counter of a code generated for the current step,
// allowing the clock to cross one step boundary while the test runs.
func currentTOTPStep() any {
	return mock.MatchedBy(func(counter int64) bool {
		now := time.Now().Unix() / 30
		return counter == now || counter == now-1
	})
}

// Issue #442: MFAEnable hands the enrollment code's counter to EnableMFA, so the
// code that turned MFA on cannot be spent again at Login.
func TestMFAEnable_RecordsEnrollmentCodeCounter(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	secret := "JBSWY3DPEHPK3PXP"
	expiresAt := time.Now().Add(mfaPendingExpiry)
	user := createTestUser(t, "SecurePass@123")
	user.MFAPendingSecret = secret
	user.MFAPendingSecretExpiresAt = &expiresAt
	step := time.Now().Unix()/30 - 1

	mockStore.On("GetUserByID", ctx, user.ID).Return(user, nil)
	mockStore.On("EnableMFA", ctx, user.ID, secret, step, mock.AnythingOfType("[]string")).Return(nil).Once()

	_, err := service.MFAEnable(ctx, user.ID, generateTOTP(secret, step))
	require.NoError(t, err)
	mockStore.AssertExpectations(t)
	mockStore.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
}
