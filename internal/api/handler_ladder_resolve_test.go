package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Handler tests for POST /api/ladder/tranches/{id}/resolve (platform#118 PR5b).
// The store is a FIXTURE; the locking and chain rules are covered against real
// PostgreSQL in internal/config and ladder_resolve_race_integration_test.go.

const resolveTrancheID = "0f0f0f0f-1111-4222-8333-444444444444"

type resolveTestStore struct {
	*MockConfigStore
	account string
	called  int
	got     config.LadderResolveFailed
	res     *config.LadderResolveResult
	err     error
}

func (s *resolveTestStore) LadderTrancheScope(context.Context, string) (string, string, error) {
	return s.account, "aws", nil
}

func (s *resolveTestStore) ResolveLadderTrancheFailed(_ context.Context, r config.LadderResolveFailed) (*config.LadderResolveResult, error) {
	s.called++
	s.got = r
	if s.res == nil && s.err == nil {
		return &config.LadderResolveResult{ID: r.TrancheID, Status: "failed", StatusReason: r.StatusReason}, nil
	}
	return s.res, s.err
}

const resolveProvenBody = `{"outcome":"failed","reason":"verified no purchase in the cloud console"}`

func resolveCall(t *testing.T, caller apgCaller, store *resolveTestStore, body string) error {
	t.Helper()
	h, req := caller.handlerAndReq(store.MockConfigStore, body)
	h.config = store
	_, err := h.resolveLadderTranche(context.Background(), req, resolveTrancheID)
	return err
}

func newResolveStore() *resolveTestStore {
	m := new(MockConfigStore)
	m.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Provider: "aws"}, nil
	}
	return &resolveTestStore{MockConfigStore: m, account: scopedInAccount}
}

func clientCode(t *testing.T, err error) int {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error, not a 500: %v", err)
	return ce.code
}

func TestLadderResolve_Authorization(t *testing.T) {
	constrained := []auth.Permission{{Action: auth.ActionExecute, Resource: auth.ResourcePurchases,
		Constraints: &auth.PermissionConstraints{AccountIDs: []string{scopedOutAccount}}}}
	tests := []struct {
		name   string
		caller apgCaller
		want   int // 0 = allowed
	}{
		{"execute:purchases user", apgCaller{perms: apgAdminPlus}, 0},
		{"admin without execute:purchases", apgCaller{perms: apgAdminOnly}, 403},
		{"admin API key", apgCaller{key: true}, 403},
		{"execute:purchases scoped to another account", apgCaller{perms: constrained}, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newResolveStore()
			err := resolveCall(t, tc.caller, store, resolveProvenBody)
			if tc.want == 0 {
				require.NoError(t, err)
				assert.Equal(t, 1, store.called)
				return
			}
			assert.Equal(t, tc.want, clientCode(t, err))
			assert.Zero(t, store.called, "a refused caller must not reach the store")
		})
	}
}

// A principal whose account list excludes the tranche's account gets the
// enumeration-safe not-found, like the amend endpoint.
func TestLadderResolve_OutOfScopeAccount(t *testing.T) {
	store := newResolveStore()
	store.account = scopedOutAccount
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: apgUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped(apgAdminPlus, []string{scopedInAccount})
	h := &Handler{config: store, auth: mockAuth}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}, Body: resolveProvenBody}
	_, err := h.resolveLadderTranche(context.Background(), req, resolveTrancheID)
	require.ErrorIs(t, err, errNotFound)
	assert.Zero(t, store.called)
}

func TestLadderResolve_BodyValidation(t *testing.T) {
	long := strings.Repeat("x", 301)
	check := func(acct, region, svc, at string) string {
		return fmt.Sprintf(`"acknowledge_unknown_outcome":true,"cloud_check":{"checked_account_id":%q,"checked_region":%q,"checked_service":%q,"checked_at":%q}`, acct, region, svc, at)
	}
	for name, body := range map[string]string{
		"empty":                ``,
		"unknown field":        `{"outcome":"failed","reason":"verified no purchase in the cloud console","actor":"x"}`,
		"two documents":        resolveProvenBody + resolveProvenBody,
		"bad outcome":          `{"outcome":"voided","reason":"verified no purchase in the cloud console"}`,
		"missing reason":       `{"outcome":"failed"}`,
		"short reason":         `{"outcome":"failed","reason":"nope"}`,
		"whitespace reason":    `{"outcome":"failed","reason":"            "}`,
		"long reason":          `{"outcome":"failed","reason":"` + long + `"}`,
		"newline in reason":    `{"outcome":"failed","reason":"verified no purchase\nin the console"}`,
		"control char in note": `{"outcome":"failed","reason":"verified no purchase in the cloud console","note":"a\u0007b"}`,
		"long note":            `{"outcome":"failed","reason":"verified no purchase in the cloud console","note":"` + strings.Repeat("n", 201) + `"}`,
		"ack without check":    `{"outcome":"failed","reason":"verified no purchase in the cloud console","acknowledge_unknown_outcome":true}`,
		"check without ack":    `{"outcome":"failed","reason":"verified no purchase in the cloud console","cloud_check":{}}`,
		"bad checked_at":       `{"outcome":"failed","reason":"verified no purchase in the cloud console",` + check("acct-example-1", "us-east-1", "compute-sp", "yesterday") + `}`,
		"quote in account":     `{"outcome":"failed","reason":"verified no purchase in the cloud console",` + check(`1"2`, "us-east-1", "compute-sp", "2026-01-01T00:00:00Z") + `}`,
		"upper-case region":    `{"outcome":"failed","reason":"verified no purchase in the cloud console",` + check("acct-example-1", "US East", "compute-sp", "2026-01-01T00:00:00Z") + `}`,
		"oversized body":       `{"outcome":"failed","reason":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newResolveStore()
			err := resolveCall(t, apgCaller{perms: apgAdminPlus}, store, body)
			assert.Equal(t, 400, clientCode(t, err))
			assert.Zero(t, store.called)
		})
	}
}

func TestLadderResolve_CompletedIsNotImplemented(t *testing.T) {
	store := newResolveStore()
	err := resolveCall(t, apgCaller{perms: apgAdminPlus}, store, `{"outcome":"completed","reason":"verified the commitment in the console"}`)
	assert.Equal(t, 501, clientCode(t, err))
	assert.Zero(t, store.called, "completed must not touch the tranche until the visibility check exists")
}

func TestLadderResolve_StatusReasonIsDeterministic(t *testing.T) {
	proven := newResolveStore()
	require.NoError(t, resolveCall(t, apgCaller{perms: apgAdminPlus}, proven, resolveProvenBody))
	assert.Equal(t, `operator:failed:proven: reason="verified no purchase in the cloud console"`, proven.got.StatusReason)
	assert.False(t, proven.got.Acknowledge)

	unverified := newResolveStore()
	body := `{"outcome":"failed","reason":"reaped execution, checked the console","note":"ticket 42","acknowledge_unknown_outcome":true,` +
		`"cloud_check":{"checked_account_id":"acct-example-1","checked_region":"us-east-1","checked_service":"compute-sp","checked_at":"2026-10-13T10:00:00+02:00"}}`
	require.NoError(t, resolveCall(t, apgCaller{perms: apgAdminPlus}, unverified, body))
	assert.Equal(t, `operator:failed:unverified: ack=true account="acct-example-1" region="us-east-1" service="compute-sp" checked_at=2026-10-13T08:00:00Z reason="reaped execution, checked the console" note="ticket 42"`,
		unverified.got.StatusReason)
	assert.True(t, unverified.got.Acknowledge)
	assert.Equal(t, scopedInAccount, unverified.got.AccountID)
}

func TestLadderResolve_StoreErrorsMapToStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{config.ErrLadderResolveNotFound, 404},
		{config.ErrLadderResolveInvalid, 400},
		{fmt.Errorf("x: %w", config.ErrLadderResolveConflict), 409},
	} {
		store := newResolveStore()
		store.err = tc.err
		err := resolveCall(t, apgCaller{perms: apgAdminPlus}, store, resolveProvenBody)
		if tc.want == 404 {
			require.ErrorIs(t, err, errNotFound)
			continue
		}
		assert.Equal(t, tc.want, clientCode(t, err))
	}
	store := newResolveStore()
	store.err = fmt.Errorf("connection reset")
	err := resolveCall(t, apgCaller{perms: apgAdminPlus}, store, resolveProvenBody)
	require.Error(t, err)
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "an unexpected store error is a 500, not a client error")
}

func TestLadderResolve_BadIDIsNotFound(t *testing.T) {
	store := newResolveStore()
	h, req := (apgCaller{perms: apgAdminPlus}).handlerAndReq(store.MockConfigStore, resolveProvenBody)
	h.config = store
	_, err := h.resolveLadderTranche(context.Background(), req, "not-a-uuid")
	require.ErrorIs(t, err, errNotFound)
}
