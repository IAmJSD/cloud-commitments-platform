package api

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1517: the global_config write closures run inside the transaction
// UpdateGlobalConfigAtomic opens, which holds a pooled connection. Any auth
// lookup from there needs a second connection from the same pool, so enough
// concurrent arming saves deadlock the instance. These tests pin that the
// closure makes no auth call at all, while the grant outcomes stay the same.
// The real-pool reproduction is TestGlobalConfigArmingSavesDoNotExhaustPool.

// authFreeTxStore models UpdateGlobalConfigAtomic and records how many auth
// calls the apply closure made while the "transaction" was open.
type authFreeTxStore struct {
	*MockConfigStore
	auth      *MockAuthService
	stored    config.GlobalConfig
	callsInTx int
}

func (s *authFreeTxStore) UpdateGlobalConfigAtomic(_ context.Context, apply func(*config.GlobalConfig) error) (*config.GlobalConfig, error) {
	cfg := s.stored
	before := len(s.auth.Calls)
	err := apply(&cfg)
	s.callsInTx += len(s.auth.Calls) - before
	if err != nil {
		return nil, err
	}
	s.stored = cfg
	return &cfg, nil
}

func (s *authFreeTxStore) ListServiceConfigs(context.Context) ([]config.ServiceConfig, error) {
	return []config.ServiceConfig{}, nil
}

func newAuthFreeTxHandler(perms []auth.Permission, stored config.GlobalConfig) (*Handler, *authFreeTxStore, *MockAuthService) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: apgUserID}, nil).Maybe()
	mockAuth.grantPermissions(perms)
	store := &authFreeTxStore{MockConfigStore: new(MockConfigStore), auth: mockAuth, stored: stored}
	return &Handler{config: store, auth: mockAuth}, store, mockAuth
}

func authFreeReq(body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}, Body: body}
}

var (
	afAdmin   = []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}
	afGranted = []auth.Permission{
		{Action: auth.ActionAdmin, Resource: auth.ResourceAll},
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases},
		{Action: auth.ActionExecute, Resource: auth.ResourceRIExchange},
	}
)

const afRIExchangeArm = `{"auto_exchange_enabled":true,"mode":"auto","utilization_threshold":50,"max_payment_per_exchange_usd":100,"max_payment_daily_usd":500,"lookback_days":30}`

func TestGlobalConfigArmingGates_NoAuthCallInsideTransaction(t *testing.T) {
	tests := []struct {
		name        string
		perms       []auth.Permission
		dedicated   bool
		body        string
		wantRefused bool
	}{
		{"arm auto-purchase, granted", afGranted, false, `{"auto_purchase_enabled":true}`, false},
		{"arm auto-purchase, refused", afAdmin, false, `{"auto_purchase_enabled":true}`, true},
		{"arm ladder execution, granted", afGranted, false, `{"laddering_enabled":true,"ladder_execution_enabled":true}`, false},
		{"arm ladder execution, refused", afAdmin, false, `{"laddering_enabled":true,"ladder_execution_enabled":true}`, true},
		{"arm RI exchange via config, granted", afGranted, false, `{"ri_exchange_enabled":true,"ri_exchange_mode":"auto"}`, false},
		{"arm RI exchange via config, refused", afAdmin, false, `{"ri_exchange_enabled":true,"ri_exchange_mode":"auto"}`, true},
		{"arm RI exchange via dedicated route, granted", afGranted, true, afRIExchangeArm, false},
		{"arm RI exchange via dedicated route, refused", afAdmin, true, afRIExchangeArm, true},
		{"ordinary save, admin", afAdmin, false, `{"approval_required":false}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, store, _ := newAuthFreeTxHandler(tc.perms, baseValidConfig())
			var err error
			if tc.dedicated {
				_, err = h.updateRIExchangeConfig(context.Background(), authFreeReq(tc.body))
			} else {
				_, err = h.updateConfig(context.Background(), authFreeReq(tc.body))
			}
			if tc.wantRefused {
				apgRefused(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Zero(t, store.callsInTx, "the write closure must not call the auth service: it would need a second pooled connection")
		})
	}
}

// A failed execute-verb lookup is raised only when the write escalates, exactly
// as when the lookup ran inside the closure on escalation alone. Resolving it
// up front must not fail an ordinary save, nor turn a lookup error into a pass.
func TestGlobalConfigArmingGates_LookupErrorOnlyMattersOnEscalation(t *testing.T) {
	lookupErr := errors.New("auth store unavailable")
	newHandler := func() (*Handler, *authFreeTxStore) {
		mockAuth := new(MockAuthService)
		mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: apgUserID}, nil).Maybe()
		mockAuth.On("HasPermissionAPI", mock.Anything, apgUserID, auth.ActionExecute, mock.Anything).Return(false, lookupErr)
		mockAuth.grantPermissions(afAdmin)
		store := &authFreeTxStore{MockConfigStore: new(MockConfigStore), auth: mockAuth, stored: baseValidConfig()}
		return &Handler{config: store, auth: mockAuth}, store
	}

	t.Run("ordinary save succeeds", func(t *testing.T) {
		h, store := newHandler()
		_, err := h.updateConfig(context.Background(), authFreeReq(`{"approval_required":false}`))
		require.NoError(t, err)
		assert.False(t, store.stored.ApprovalRequired)
	})
	for _, body := range []string{
		`{"auto_purchase_enabled":true}`,
		`{"laddering_enabled":true,"ladder_execution_enabled":true}`,
		`{"ri_exchange_enabled":true,"ri_exchange_mode":"auto"}`,
	} {
		t.Run("escalation fails closed: "+body, func(t *testing.T) {
			h, store := newHandler()
			_, err := h.updateConfig(context.Background(), authFreeReq(body))
			require.ErrorIs(t, err, lookupErr)
			assert.Equal(t, baseValidConfig(), store.stored, "a refused write must not land")
		})
	}
	t.Run("dedicated route escalation fails closed", func(t *testing.T) {
		h, store := newHandler()
		_, err := h.updateRIExchangeConfig(context.Background(), authFreeReq(afRIExchangeArm))
		require.ErrorIs(t, err, lookupErr)
		assert.Equal(t, baseValidConfig(), store.stored)
	})
}
