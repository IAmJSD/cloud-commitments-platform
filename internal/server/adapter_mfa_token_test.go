package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Issue #134: the MFA continuation token end to end through the router, the
// login handler, authServiceAdapter and the real *auth.Service (store mocked).
// The password step answers mfa_required with an mfa_token; the code step
// sends {email, mfa_token, mfa_code} with no password and gets a session.
func TestLogin_MFATokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := new(auth.MockStore)
	realAuth := auth.NewService(auth.ServiceConfig{
		Store: store, SessionDuration: time.Hour, CSRFKey: auth.TestCSRFKey(),
		DashboardURL: "http://localhost:3000", BcryptCost: bcrypt.MinCost,
	})
	router := api.NewRouter(api.NewHandler(api.HandlerConfig{AuthService: newAuthServiceAdapter(realAuth)}))

	const email = "mfa-round-trip@example.com"
	const rtSecret = "RoundTripPass@1" // test-only placeholder
	const recoveryCode = "ABCD-EFGH"
	user := &auth.User{
		ID: "user-rt", Email: email, Active: true, PasswordHash: auth.TestPasswordHash(t, rtSecret),
		MFAEnabled: true, MFASecret: "JBSWY3DPEHPK3PXP", PasswordVersion: 4,
		MFARecoveryCodes: []string{auth.TestPasswordHash(t, "ABCDEFGH")},
	}
	store.On("GetUserByEmail", ctx, email).Return(user, nil).Once()

	var storedHash string
	store.On("CreateMFALoginToken", ctx, mock.AnythingOfType("string"), user.ID, int64(4), auth.MFALoginTokenTTL).
		Run(func(args mock.Arguments) { storedHash = args.String(1) }).
		Return(time.Now().Add(auth.MFALoginTokenTTL), nil).Once()

	post := func(body string) (any, error) {
		req := &events.LambdaFunctionURLRequest{Body: body}
		req.RequestContext.HTTP.SourceIP = "127.0.0.1"
		return router.Route(ctx, "POST", "/api/auth/login", req)
	}

	pw := base64.StdEncoding.EncodeToString([]byte(rtSecret))
	_, err := post(`{"email":"` + email + `","password":"` + pw + `"}`)
	ce, ok := api.IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	require.Equal(t, "mfa_required", ce.Error())
	details := ce.Details()
	token, _ := details["mfa_token"].(string)
	require.NotEmpty(t, token, "the mfa_required answer carries the token")
	assert.NotEmpty(t, details["mfa_token_expires_at"])
	assert.Equal(t, hashSessionTokenForTest(token), storedHash, "only the token's hash is stored")

	store.On("ClaimMFALoginTokenAttempt", ctx, storedHash, auth.MaxMFALoginTokenAttempts).
		Return(&auth.MFALoginToken{UserID: user.ID, PasswordVersion: 4, Attempts: 1}, nil).Once()
	store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()
	store.On("ConsumeMFARecoveryCode", ctx, user.ID, mock.Anything, mock.Anything).Return(nil).Once()
	store.On("ConsumeMFALoginToken", ctx, storedHash).Return(true, nil).Once()
	store.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil).Once()
	store.On("RecordSuccessfulLogin", ctx, user.ID).Return(nil).Once()

	body, err := json.Marshal(map[string]string{"email": email, "mfa_token": token, "mfa_code": recoveryCode})
	require.NoError(t, err)
	result, err := post(string(body))
	require.NoError(t, err)
	resp, ok := result.(*api.LoginResponse)
	require.True(t, ok)
	assert.NotEmpty(t, resp.Token)
	assert.Equal(t, email, resp.User.Email)
	store.AssertExpectations(t)
}
