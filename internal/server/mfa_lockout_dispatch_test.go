package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

const (
	mfaLockoutUserID   = "88888888-8888-4888-8888-888888888888"
	mfaLockoutToken    = "mfa-lockout-session-token"
	mfaLockoutPassword = "CorrectPassword123"
)

// A session plus the password must not be able to guess MFA codes on
// /api/auth/mfa/disable forever (issue #442). The store stub applies the same
// counter-and-lock rule as PostgresStore.RecordFailedLogin so the whole path
// (router, handler, service, store contract) is driven end to end.
func TestHandleRequest_MFADisableWrongCodesLockAccount(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte(mfaLockoutPassword), bcrypt.MinCost)
	require.NoError(t, err)
	user := &auth.User{
		ID: mfaLockoutUserID, Email: "mfa-lockout@example.com", Active: true,
		PasswordHash: string(hash), MFAEnabled: true, MFASecret: "JBSWY3DPEHPK3PXP",
		GroupIDs: []string{auth.DefaultAdminGroupID},
	}

	mockStore := new(auth.MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetSession", mock.Anything, mock.AnythingOfType("string")).Return(&auth.Session{
		Token: hashSessionTokenForTest(mfaLockoutToken), UserID: mfaLockoutUserID,
		Email: user.Email, ExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	mockStore.On("GetUserByID", mock.Anything, mfaLockoutUserID).Return(user, nil)
	failures := 0
	mockStore.On("RecordFailedLogin", mock.Anything, mfaLockoutUserID).Return(nil).Run(func(mock.Arguments) {
		failures++
		if failures >= auth.MaxFailedLoginAttempts {
			until := time.Now().Add(auth.AccountLockoutDuration)
			user.LockedUntil = &until
		}
	})

	service := auth.NewService(auth.ServiceConfig{Store: mockStore, SessionDuration: time.Hour, CSRFKey: auth.TestCSRFKey()})
	handler := api.NewHandler(api.HandlerConfig{AuthService: newAuthServiceAdapter(service), RateLimiter: api.NewInMemoryRateLimiter()})

	disable := func(code string) *events.LambdaFunctionURLResponse {
		body, err := json.Marshal(map[string]string{
			"password": base64.StdEncoding.EncodeToString([]byte(mfaLockoutPassword)),
			"code":     code,
		})
		require.NoError(t, err)
		resp, err := handler.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
			Headers: map[string]string{
				"Authorization": "Bearer " + mfaLockoutToken,
				"X-CSRF-Token":  auth.DeriveTestCSRFToken(mfaLockoutToken),
				"Content-Type":  "application/json",
			},
			Body: string(body),
			RequestContext: events.LambdaFunctionURLRequestContext{
				HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST", Path: "/api/auth/mfa/disable"},
			},
		})
		require.NoError(t, err)
		return resp
	}

	// Non-numeric, so it can neither match a TOTP window nor a recovery code.
	for i := 0; i < auth.MaxFailedLoginAttempts; i++ {
		resp := disable("not-a-code")
		assert.Equal(t, 400, resp.StatusCode, "attempt %d body: %s", i+1, resp.Body)
	}
	require.NotNil(t, user.LockedUntil, "the account must be locked after %d wrong codes", auth.MaxFailedLoginAttempts)

	// Locked: refused before any code is checked, so no TOTP claim and no disable write.
	resp := disable("not-a-code")
	assert.Equal(t, 401, resp.StatusCode, "body: %s", resp.Body)
	mockStore.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "DisableMFA", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Equal(t, auth.MaxFailedLoginAttempts, failures, "a refused request must not extend the count")
}
