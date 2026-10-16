package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
)

// A caller with no session must get the same answer for a missing and an
// existing execution on the email-link routes, so the routes are not an
// existence oracle (issue #435).
func TestHandleRequest_TokenActions_UnauthenticatedCannotTellMissingFromExisting(t *testing.T) {
	future := time.Now().Add(time.Hour)
	hash := config.HashApprovalToken(tokenErrRawToken)

	headerVariants := map[string]map[string]string{
		"no-header":      {},
		"invalid-bearer": {"authorization": "Bearer bad-tok"},
		"bogus-x-auth":   {"x-authorization": "Basic bad-tok"},
	}
	for _, action := range []string{"approve", "cancel", "revoke"} {
		for _, token := range []string{"", tokenErrRawToken, "wrong"} {
			for variant, headers := range headerVariants {
				t.Run(action+"/token="+token+"/"+variant, func(t *testing.T) {
					call := func(h *Handler) *events.LambdaFunctionURLResponse {
						req := tokenErrRequest(action, token)
						req.Headers = headers
						resp, err := h.HandleRequest(context.Background(), req)
						require.NoError(t, err)
						return resp
					}

					exec := &config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &future}
					existingH := tokenErrHandler(t, exec, nil)
					missingH := tokenErrHandler(t, nil, config.ErrNotFound)
					for _, h := range []*Handler{existingH, missingH} {
						h.auth.(*MockAuthService).On("ValidateSession", mock.Anything, "bad-tok").Return(nil, errors.New("invalid session")).Maybe()
					}
					existing := call(existingH)
					missing := call(missingH)

					assert.Equal(t, 401, existing.StatusCode, existing.Body)
					assert.Equal(t, existing.StatusCode, missing.StatusCode, "missing: %s", missing.Body)
					assert.Equal(t, existing.Body, missing.Body)
				})
			}
		}
	}
}

// oracleCSRF is the CSRF header value the signed-in callers below send when
// the variant says they send a valid one.
const oracleCSRF = "csrf-ok"

// oracleCaller is a signed-in caller who may not act on the execution, or
// who may but forgot the CSRF header.
type oracleCaller struct {
	grant    func(*MockAuthService)
	email    string
	sendCSRF bool
}

func oracleCallers() map[string]oracleCaller {
	none := func(m *MockAuthService) { m.grantPermissions(nil) }
	own := func(m *MockAuthService) {
		m.grantPermissions([]auth.Permission{
			{Action: auth.ActionApproveOwn, Resource: auth.ResourcePurchases},
			{Action: auth.ActionCancelOwn, Resource: auth.ResourcePurchases},
		})
	}
	return map[string]oracleCaller{
		"no-rbac/csrf":         {grant: none, email: "outsider@example.com", sendCSRF: true},
		"no-rbac/no-csrf":      {grant: none, email: "outsider@example.com"},
		"own-not-creator/csrf": {grant: own, email: "outsider@example.com", sendCSRF: true},
		"scoped-out/csrf":      {grant: func(m *MockAuthService) { m.grantScoped("other-acct") }, email: "outsider@example.com", sendCSRF: true},
		"admin/no-csrf":        {grant: func(m *MockAuthService) { m.grantAdmin() }, email: "admin@example.com"},
		"contact/no-csrf":      {grant: none, email: "contact@example.com"},
	}
}

// oracleHandler wires a handler whose execution belongs to acct-1 (contact
// contact@example.com) and was created by another user; an orphan is an
// Azure execution whose account is gone (issue #609). exec == nil makes
// the store answer ErrNotFound. Every status write loses its CAS, so a caller
// that wrongly got past authorization sees a 409 and no purchase runs.
func oracleHandler(t *testing.T, caller oracleCaller, exec *config.PurchaseExecution, orphan bool) *Handler {
	t.Helper()
	accountID := "acct-1"
	creator := "creator-user-id"
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Name: id, ContactEmail: "contact@example.com"}, nil
	}
	if exec == nil {
		store.On("GetExecutionByID", mock.Anything, tokenErrExecID).Return(nil, config.ErrNotFound)
	} else {
		exec.ExecutionID = tokenErrExecID
		exec.CloudAccountID = &accountID
		exec.CreatedByUserID = &creator
		exec.Recommendations = []config.RecommendationRecord{{ID: "r1", Provider: "aws", CloudAccountID: &accountID, UpfrontCost: 100}}
		if orphan {
			exec.CloudAccountID = nil
			exec.Recommendations = []config.RecommendationRecord{{ID: "r1", Provider: "azure", UpfrontCost: 100}}
		}
		store.On("GetExecutionByID", mock.Anything, tokenErrExecID).Return(exec, nil)
		store.On("TransitionExecutionStatus", mock.Anything, tokenErrExecID, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, config.ErrExecutionNotInExpectedStatus).Maybe()
		store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, tokenErrExecID, mock.Anything).
			Return(false, exec.Status, nil).Maybe()
	}
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()

	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: "caller-user-id", Email: caller.email}, nil)
	m.On("ValidateCSRFToken", mock.Anything, "sess-tok", oracleCSRF).Return(nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, "sess-tok", "").Return(errors.New("missing CSRF token")).Maybe()
	caller.grant(m)

	return &Handler{
		config:   store,
		auth:     m,
		purchase: purchase.NewManager(purchase.ManagerConfig{ConfigStore: store}),
	}
}

// A signed-in caller who may not act on an execution, or who omits the CSRF
// header, must get the same status, headers and body for an existing and a
// missing execution ID on approve, cancel and revoke (issue #597).
func TestHandleRequest_TokenActions_SignedInCannotTellMissingFromExisting(t *testing.T) {
	future := time.Now().Add(time.Hour)
	hash := config.HashApprovalToken(tokenErrRawToken)

	for _, action := range []string{"approve", "cancel", "revoke"} {
		for _, status := range []string{"pending", "completed", "orphan"} {
			for _, token := range []string{"", tokenErrRawToken, "wrong"} {
				for name, caller := range oracleCallers() {
					if name == "contact/no-csrf" && token == "" {
						continue // without a token the contact gate never runs
					}
					t.Run(action+"/"+status+"/token="+token+"/"+name, func(t *testing.T) {
						call := func(h *Handler) *events.LambdaFunctionURLResponse {
							req := tokenErrRequest(action, token)
							if caller.sendCSRF {
								req.Headers["x-csrf-token"] = oracleCSRF
							}
							resp, err := h.HandleRequest(context.Background(), req)
							require.NoError(t, err)
							return resp
						}
						orphan := status == "orphan"
						execStatus := status
						if orphan {
							execStatus = "pending"
						}
						exec := &config.PurchaseExecution{Status: execStatus, ApprovalToken: hash, ApprovalTokenExpiresAt: &future}
						existing := call(oracleHandler(t, caller, exec, orphan))
						missing := call(oracleHandler(t, caller, nil, false))

						assert.Contains(t, []int{403, 404}, existing.StatusCode, existing.Body)
						assert.Equal(t, missing.StatusCode, existing.StatusCode, "existing: %s / missing: %s", existing.Body, missing.Body)
						assert.Equal(t, missing.Body, existing.Body)
						assert.Equal(t, missing.Headers, existing.Headers)
					})
				}
			}
		}
	}
}

// requireConcealedDenial asserts err answers like a missing execution (404
// "execution not found") and that the denial it hides contains wantCause,
// so a test still pins which gate denied the caller (issue #597).
func requireConcealedDenial(t *testing.T, err error, wantCause string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got %v", err)
	assert.Equal(t, 404, ce.code)
	assert.Equal(t, "execution not found", ce.message)
	var cd *concealedDenialError
	require.ErrorAs(t, err, &cd)
	assert.Contains(t, cd.cause.Error(), wantCause)
}
