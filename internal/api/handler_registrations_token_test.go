package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// reference_token on the admin registration endpoints (issue #310, finding
// A02-020).
//
// The reference token is the registrant's only credential for
// GET /api/register/{token}. rejectRegistration already built a filtered map
// so the token was "not exposed to admin", but listRegistrations and
// getRegistration returned config.AccountRegistration as-is, and its
// ReferenceToken field was tagged json:"reference_token".

const (
	regTokenSession = "reg-token-admin"
	regTokenID      = "22222222-2222-2222-2222-222222222222"
	regTokenValue   = "reg-token-value-under-test"
)

func regTokenHandler(t *testing.T) (*Handler, *MockConfigStore) {
	t.Helper()
	store := new(MockConfigStore)
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("ValidateSession", mock.Anything, regTokenSession).
		Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	m.grantAdmin()
	return &Handler{config: store, auth: m}, store
}

func regTokenFixture() config.AccountRegistration {
	return config.AccountRegistration{
		ID: regTokenID, ReferenceToken: regTokenValue, Status: "pending", Provider: "aws",
		AccountName: "Registrant", ContactEmail: "owner@example.com",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func regTokenGet(t *testing.T, h *Handler, path string) *events.LambdaFunctionURLResponse {
	t.Helper()
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer " + regTokenSession},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: path},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, resp.Body)
	return resp
}

// assertNoReferenceToken checks the raw body for both the key and the value,
// and that the registration itself is still there (a fix that returns nothing
// would also hide the token).
func assertNoReferenceToken(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, "reference_token", "admin response exposes the reference_token key")
	assert.NotContains(t, body, regTokenValue, "admin response exposes the registrant's token value")
	assert.Contains(t, body, regTokenID, "admin response must still carry the registration")
}

func TestListRegistrations_DoesNotExposeReferenceToken(t *testing.T) {
	h, store := regTokenHandler(t)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("ListAccountRegistrations", mock.Anything, mock.Anything).
		Return([]config.AccountRegistration{regTokenFixture()}, nil)

	resp := regTokenGet(t, h, "/api/registrations")
	assertNoReferenceToken(t, resp.Body)

	var regs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &regs))
	require.Len(t, regs, 1)
	assert.Equal(t, "pending", regs[0]["status"])
}

func TestGetRegistration_DoesNotExposeReferenceToken(t *testing.T) {
	h, store := regTokenHandler(t)
	t.Cleanup(func() { store.AssertExpectations(t) })
	reg := regTokenFixture()
	store.On("GetAccountRegistration", mock.Anything, regTokenID).Return(&reg, nil)

	resp := regTokenGet(t, h, "/api/registrations/"+regTokenID)
	assertNoReferenceToken(t, resp.Body)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &got))
	assert.Equal(t, "Registrant", got["account_name"])
}

// TestAccountRegistration_JSONOmitsReferenceToken pins the type itself, so
// any future endpoint that serializes a registration inherits the redaction.
func TestAccountRegistration_JSONOmitsReferenceToken(t *testing.T) {
	b, err := json.Marshal(regTokenFixture())
	require.NoError(t, err)
	assert.NotContains(t, string(b), "reference_token")
	assert.NotContains(t, string(b), regTokenValue)
}
