package api //nolint:revive // var-naming: package name "api" is intentional for handler package

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1005, helper level: each purchase authorization helper must decide
// a user-API-key session's full-scope *-any verb on the KEY (intersected
// with its owner), never on the owner alone, so that the verb check and the
// Constraints check that follows judge the same principal. Apart from
// direct execute, no route hands these helpers a key session today
// (TestSessionOnlyPurchaseRoutes_RefuseAUserAPIKey), so this pins the
// helpers themselves.

var errOKPUnbound = errors.New("no held approve verb binds this row")

func okpRecs() []config.RecommendationRecord {
	return []config.RecommendationRecord{{
		ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1", Count: 1, UpfrontCost: 100,
	}}
}

func okpExecution(creator string) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID: okpRowID, Status: "failed", CreatedByUserID: &creator, Recommendations: okpRecs(),
	}
}

type okpHelperCase struct {
	name, anyVerb, ownVerb string
	// narrow is a key scope holding no full-scope verb; defaults to ownVerb.
	narrow []string
	call   func(h *Handler, s *Session, creator string) error
}

var okpHelperCases = []okpHelperCase{
	{name: "approve", anyVerb: auth.ActionApproveAny, ownVerb: auth.ActionApproveOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionApprove(context.Background(), s, okpExecution(creator))
		}},
	{name: "approve token constraints", anyVerb: auth.ActionApproveAny, ownVerb: auth.ActionApproveOwn,
		call: func(h *Handler, s *Session, creator string) error {
			bound, err := h.enforceHeldApproveConstraints(context.Background(), s, okpExecution(creator))
			if err == nil && !bound {
				return errOKPUnbound
			}
			return err
		}},
	{name: "execute direct", anyVerb: auth.ActionExecuteAny, ownVerb: auth.ActionExecuteOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionExecuteDirect(context.Background(), s, creator, okpRecs())
		}},
	{name: "cancel", anyVerb: auth.ActionCancelAny, ownVerb: auth.ActionCancelOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionCancel(context.Background(), s, okpExecution(creator))
		}},
	{name: "retry", anyVerb: auth.ActionRetryAny, ownVerb: auth.ActionRetryOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionRetry(context.Background(), s, okpExecution(creator))
		}},
	{name: "revoke scheduled", anyVerb: auth.ActionRevokeAny, ownVerb: auth.ActionRevokeOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionRevokeExecution(context.Background(), s, okpExecution(creator))
		}},
	// History rows carry no creator (ownership is account access, and the
	// owner's scope is unrestricted here), so the narrow key holds no revoke
	// verb at all and creator is ignored.
	{name: "revoke history", anyVerb: auth.ActionRevokeAny, ownVerb: auth.ActionRevokeOwn, narrow: []string{auth.ActionExecute},
		call: func(h *Handler, s *Session, _ string) error {
			acct := "acct-1005"
			return h.authorizeSessionRevoke(context.Background(), s, &config.PurchaseHistoryRecord{
				PurchaseID: okpRowID, CloudAccountID: &acct, Provider: "azure", Service: "compute", Region: "westeurope",
			})
		}},
	// The pre-fetch probe has no row, so the narrow key holds no approve verb.
	{name: "ri exchange approve right", anyVerb: auth.ActionApproveAny, ownVerb: auth.ActionApproveOwn, narrow: []string{auth.ActionExecute},
		call: func(h *Handler, s *Session, _ string) error {
			return h.sessionHasApproveRight(context.Background(), s)
		}},
	{name: "ri exchange approve", anyVerb: auth.ActionApproveAny, ownVerb: auth.ActionApproveOwn,
		call: func(h *Handler, s *Session, creator string) error {
			return h.authorizeSessionApproveRIExchange(context.Background(), s, &config.RIExchangeRecord{
				ID: okpRowID, Status: "pending", CreatedByUserID: &creator,
			})
		}},
}

func okpHelperHandler(a AuthServiceInterface) *Handler {
	store := new(MockConfigStore)
	store.On("ListCloudAccounts", mock.Anything, mock.Anything).Return([]config.CloudAccount{}, nil).Maybe()
	return &Handler{auth: a, config: store}
}

func okpKeySession() *Session { return &Session{UserID: okpOwnerID, UserAPIKeyID: okpKeyID} }

// TestPurchaseAuthzHelpers_FullScopeFollowsTheAPIKey: the owner holds every
// *-any verb throughout, so a helper that probed the owner would let the
// narrow key through (or wrongly route its own-row request to *-any).
func TestPurchaseAuthzHelpers_FullScopeFollowsTheAPIKey(t *testing.T) {
	for _, tc := range okpHelperCases {
		narrow := tc.narrow
		if narrow == nil {
			narrow = []string{tc.ownVerb}
		}
		t.Run(tc.name+"/narrow key on another user's row is refused", func(t *testing.T) {
			err := tc.call(okpHelperHandler(newOKPAuth(narrow...)), okpKeySession(), okpVictimID)
			assert.Error(t, err)
		})
		t.Run(tc.name+"/key with neither verb on its owner's row is refused", func(t *testing.T) {
			err := tc.call(okpHelperHandler(newOKPAuth(auth.ActionExecute)), okpKeySession(), okpOwnerID)
			assert.Error(t, err)
		})
		t.Run(tc.name+"/key "+tc.ownVerb+" on its owner's row is allowed", func(t *testing.T) {
			err := tc.call(okpHelperHandler(newOKPAuth(tc.ownVerb)), okpKeySession(), okpOwnerID)
			assert.NoError(t, err)
		})
		t.Run(tc.name+"/key "+tc.anyVerb+" on another user's row is allowed", func(t *testing.T) {
			err := tc.call(okpHelperHandler(newOKPAuth(tc.anyVerb)), okpKeySession(), okpVictimID)
			assert.NoError(t, err)
		})
		t.Run(tc.name+"/owner session on another user's row is allowed", func(t *testing.T) {
			err := tc.call(okpHelperHandler(newOKPAuth()), &Session{UserID: okpOwnerID, Email: "owner@example.com"}, okpVictimID)
			assert.NoError(t, err)
		})
	}
}

// TestPurchaseAuthzHelpers_APIKeyVerbProbeErrorRefuses: an error probing
// the key's *-any verb, or its *-own verb once *-any is not held, is
// returned, never read as "not held". Every other answer allows, including
// the owner's own grants, so a helper that swallowed the error or probed the
// owner would succeed instead.
func TestPurchaseAuthzHelpers_APIKeyVerbProbeErrorRefuses(t *testing.T) {
	probe := []auth.PermissionConstraints{{}}
	for _, tc := range okpHelperCases {
		for _, failing := range []string{tc.anyVerb, tc.ownVerb} {
			t.Run(tc.name+"/"+failing, func(t *testing.T) {
				boom := errors.New("key store down")
				m := new(MockAuthService)
				if failing == tc.ownVerb {
					m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, okpKeyID, okpOwnerID, tc.anyVerb, auth.ResourcePurchases, probe).
						Return(false, nil)
				}
				m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, okpKeyID, okpOwnerID, failing, auth.ResourcePurchases, probe).
					Return(false, boom)
				m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, okpKeyID, okpOwnerID, mock.Anything, auth.ResourcePurchases,
					mock.Anything).Return(true, nil).Maybe()
				m.On("HasPermissionAPI", mock.Anything, okpOwnerID, mock.Anything, auth.ResourcePurchases).Return(true, nil).Maybe()
				m.On("GetAllowedAccountsAPI", mock.Anything, okpOwnerID).Return([]string{}, nil).Maybe()

				err := tc.call(okpHelperHandler(m), okpKeySession(), okpOwnerID)
				require.ErrorIs(t, err, boom)
			})
		}
	}
}

// TestPurchaseAuthzHelpers_AdminAPIKeyUnchanged: the stateless admin API key
// still short-circuits every helper before any permission probe.
func TestPurchaseAuthzHelpers_AdminAPIKeyUnchanged(t *testing.T) {
	for _, tc := range okpHelperCases {
		if tc.name == "approve token constraints" {
			continue // approveConstraintsForSession exempts the admin key before this helper runs
		}
		t.Run(tc.name, func(t *testing.T) {
			m := new(MockAuthService) // no expectations: any probe would panic
			err := tc.call(okpHelperHandler(m), &Session{UserID: apiKeyAdminUserID}, okpVictimID)
			assert.NoError(t, err)
			assert.Empty(t, m.Calls)
		})
	}
}
