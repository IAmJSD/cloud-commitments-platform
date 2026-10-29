package api

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Coverage for the auto-purchase arming gate (platform#631 R1b). Arming a plan
// or the global kill switch lets plan steps buy with no approver, so it needs
// execute:purchases (carved out of admin:*), and the stateless admin API key,
// which skips every permission check, is refused outright. De-escalations stay
// on the ordinary verbs.

const (
	apgPlanID  = "11111111-1111-1111-1111-111111111111"
	apgAdminKy = "apg-admin-key"
	apgUserID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

var (
	apgAdminOnly = []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}
	apgAdminPlus = []auth.Permission{
		{Action: auth.ActionAdmin, Resource: auth.ResourceAll},
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases},
	}
)

func apgCap(v float64) *float64 { return &v }

func apgStoredPlan(armed bool, capUSD *float64) *config.PurchasePlan {
	return &config.PurchasePlan{
		ID: apgPlanID, Name: "p", Enabled: true, AutoPurchase: armed, MaxStepCommitment: capUSD,
		NotificationDaysBefore: 3, CreatedAt: time.Now().AddDate(0, -1, 0),
		Services: map[string]config.ServiceConfig{"aws/rds": {Provider: "aws", Service: "rds"}},
	}
}

// apgCaller: "" = admin API key, otherwise a bearer session holding perms.
type apgCaller struct {
	perms []auth.Permission
	key   bool
}

func (c apgCaller) handlerAndReq(store *MockConfigStore, body string) (*Handler, *events.LambdaFunctionURLRequest) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: apgUserID}, nil).Maybe()
	mockAuth.grantPermissions(c.perms)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return([]string{"*"}, nil).Maybe()
	h := &Handler{config: store, auth: mockAuth}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}, Body: body}
	if c.key {
		h.apiKey = apgAdminKy
		req.Headers = map[string]string{"x-api-key": apgAdminKy}
	}
	return h, req
}

func apgCalled(m *MockConfigStore, method string) bool {
	for _, c := range m.Calls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func apgRefused(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error, not a 500: %v", err)
	assert.Equal(t, 403, ce.code)
}

func TestAutoPurchaseGate_PlanWrites(t *testing.T) {
	admin := apgCaller{perms: apgAdminOnly}
	granted := apgCaller{perms: apgAdminPlus}
	key := apgCaller{key: true}

	tests := []struct {
		name        string
		caller      apgCaller
		method      string // patch | put
		stored      *config.PurchasePlan
		body        string
		wantRefused bool
	}{
		{"patch arms a capped plan: admin without grant refused", admin, "patch", apgStoredPlan(false, apgCap(1000)), `{"auto_purchase":true}`, true},
		{"patch arms a capped plan: execute:purchases allowed", granted, "patch", apgStoredPlan(false, apgCap(1000)), `{"auto_purchase":true}`, false},
		{"patch arms a capped plan: admin API key refused", key, "patch", apgStoredPlan(false, apgCap(1000)), `{"auto_purchase":true}`, true},
		{"patch enables a disabled armed-shaped plan: refused", admin, "patch", func() *config.PurchasePlan {
			p := apgStoredPlan(true, apgCap(1000))
			p.Enabled = false
			return p
		}(), `{"enabled":true}`, true},
		{"patch disarm with admin API key allowed", key, "patch", apgStoredPlan(true, apgCap(1000)), `{"auto_purchase":false}`, false},
		{"patch rename of armed plan by admin allowed", admin, "patch", apgStoredPlan(true, apgCap(1000)), `{"name":"x"}`, false},
		{"patch auto_purchase on a plan with NULL cap is not armed: allowed", admin, "patch", apgStoredPlan(false, nil), `{"auto_purchase":true}`, false},
		{"put raises cap: admin refused", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":2000}`, true},
		{"put raises cap: admin API key refused", key, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":2000}`, true},
		{"put raises cap: execute:purchases allowed", granted, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":2000}`, false},
		{"put lowers cap: admin allowed", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":500}`, false},
		{"put same cap: admin allowed", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":1000}`, false},
		{"put 1000.0 is the same cap: admin allowed", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":1000.0}`, false},
		{"put 100.10 (binary-float trap) over 100.10 is the same cap: admin allowed", admin, "put", apgStoredPlan(true, apgCap(100.10)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":100.10}`, false},
		{"put one cent more is a raise: admin refused", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true,"max_step_commitment":1000.01}`, true},
		{"put omitting the cap keeps it: admin allowed", admin, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":true}`, false},
		{"put disarms with admin API key allowed", key, "put", apgStoredPlan(true, apgCap(1000)), `{"name":"p","enabled":true,"auto_purchase":false}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("GetPurchasePlan", mock.Anything, apgPlanID).Return(tc.stored, nil)
			store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
			h, req := tc.caller.handlerAndReq(store, tc.body)
			var err error
			if tc.method == "patch" {
				_, err = h.patchPlan(context.Background(), req, apgPlanID)
			} else {
				_, err = h.updatePlan(context.Background(), req, apgPlanID)
			}
			if tc.wantRefused {
				apgRefused(t, err)
				assert.False(t, apgCalled(store, "UpdatePurchasePlan"), "a refused write must not reach the store")
				return
			}
			require.NoError(t, err)
			assert.True(t, apgCalled(store, "UpdatePurchasePlan"))
		})
	}
}

func TestAutoPurchaseGate_CreatePlan(t *testing.T) {
	acct := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	armedBody := `{"name":"n","enabled":true,"auto_purchase":true,"max_step_commitment":1000,"provider":"aws","service":"rds","target_accounts":["` + acct + `"]}`
	for _, tc := range []struct {
		name        string
		caller      apgCaller
		wantRefused bool
	}{
		{"admin without grant refused", apgCaller{perms: apgAdminOnly}, true},
		{"admin API key refused", apgCaller{key: true}, true},
		{"execute:purchases allowed", apgCaller{perms: apgAdminPlus}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("CreatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
			store.On("SetPlanAccounts", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
				return &config.CloudAccount{ID: id, Provider: "aws"}, nil
			}
			h, req := tc.caller.handlerAndReq(store, armedBody)
			_, err := h.createPlan(context.Background(), req)
			if tc.wantRefused {
				apgRefused(t, err)
				assert.False(t, apgCalled(store, "CreatePurchasePlan"))
				return
			}
			require.NoError(t, err)
			assert.True(t, apgCalled(store, "CreatePurchasePlan"))
		})
	}
}

func TestAutoPurchaseGate_CapValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		// decodeRefused: the body does not decode, so the stored plan is
		// never read.
		decodeRefused bool
	}{
		{"zero", `{"name":"p","max_step_commitment":0}`, false},
		{"negative", `{"name":"p","max_step_commitment":-5}`, false},
		{"below one cent", `{"name":"p","max_step_commitment":0.004}`, false},
		{"1e308 overflows the column", `{"name":"p","max_step_commitment":1e308}`, false},
		{"quoted number", `{"name":"p","max_step_commitment":"100"}`, true},
		{"half cent 1.005", `{"name":"p","max_step_commitment":1.005}`, false},
		{"0.015", `{"name":"p","max_step_commitment":0.015}`, false},
		{"1.0049", `{"name":"p","max_step_commitment":1.0049}`, false},
		{"1e-3", `{"name":"p","max_step_commitment":1e-3}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			planRead := store.On("GetPurchasePlan", mock.Anything, apgPlanID).Return(apgStoredPlan(false, nil), nil)
			if tc.decodeRefused {
				planRead.Maybe()
			}
			store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
			h, req := apgCaller{perms: apgAdminPlus}.handlerAndReq(store, tc.body)
			_, err := h.updatePlan(context.Background(), req, apgPlanID)
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 400, ce.code)
			assert.False(t, apgCalled(store, "UpdatePurchasePlan"))
		})
	}
}

// apgConfigCall runs PUT /api/config logic and returns the config handed to
// SaveGlobalConfig (nil when nothing was saved).
func apgConfigCall(t *testing.T, caller apgCaller, stored config.GlobalConfig, body string) (*config.GlobalConfig, error) {
	t.Helper()
	store := autoGateStore(t, stored)
	var saved *config.GlobalConfig
	store.ExpectedCalls = nil
	seeded := stored
	store.On("GetGlobalConfig", mock.Anything).Return(&seeded, nil).Maybe()
	store.On("ListServiceConfigs", mock.Anything).Return([]config.ServiceConfig{}, nil).Maybe()
	store.On("SaveGlobalConfig", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		c := *(a.Get(1).(*config.GlobalConfig))
		saved = &c
	}).Return(nil).Maybe()
	h, req := caller.handlerAndReq(store, body)
	_, err := h.updateConfig(context.Background(), req)
	return saved, err
}

func TestAutoPurchaseGate_KillSwitch(t *testing.T) {
	off, on := baseValidConfig(), baseValidConfig()
	on.AutoPurchaseEnabled = true

	t.Run("off to on: admin without grant refused", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, off, `{"auto_purchase_enabled":true}`)
		apgRefused(t, err)
		assert.Nil(t, saved)
	})
	t.Run("off to on: admin API key refused", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{key: true}, off, `{"auto_purchase_enabled":true}`)
		apgRefused(t, err)
		assert.Nil(t, saved)
	})
	t.Run("off to on: execute:purchases allowed and stamped", func(t *testing.T) {
		before := time.Now().UTC().Add(-time.Second)
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminPlus}, off, `{"auto_purchase_enabled":true}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.True(t, saved.AutoPurchaseEnabled)
		require.NotNil(t, saved.AutoPurchaseChangedBy)
		assert.Equal(t, apgUserID, *saved.AutoPurchaseChangedBy)
		require.NotNil(t, saved.AutoPurchaseChangedVia)
		assert.Equal(t, string(PrincipalSession), *saved.AutoPurchaseChangedVia)
		require.NotNil(t, saved.AutoPurchaseChangedAt)
		assert.True(t, saved.AutoPurchaseChangedAt.After(before))
	})
	t.Run("on to off: admin API key allowed and stamped", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{key: true}, on, `{"auto_purchase_enabled":false}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.False(t, saved.AutoPurchaseEnabled)
		require.NotNil(t, saved.AutoPurchaseChangedVia)
		assert.Equal(t, string(PrincipalAdminAPIKey), *saved.AutoPurchaseChangedVia)
	})
	t.Run("unchanged switch: admin may save other settings, no stamp", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, on, `{"auto_purchase_enabled":true,"default_coverage":70}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.Nil(t, saved.AutoPurchaseChangedBy)
	})
}

// The stamps are json:"-": PUT /api/config unmarshals the body onto GlobalConfig,
// so a tagged field would let any admin forge who last flipped the switch. Go's
// decoder matches keys case-insensitively, so cover the case variants and the
// Go field names too.
func TestAutoPurchaseGate_StampsCannotBeForgedViaConfigBody(t *testing.T) {
	stored := baseValidConfig()
	by, via := "real-actor", "user"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stored.AutoPurchaseChangedBy, stored.AutoPurchaseChangedVia, stored.AutoPurchaseChangedAt = &by, &via, &at

	for _, body := range []string{
		`{"auto_purchase_changed_by":"mallory","auto_purchase_changed_via":"x","auto_purchase_changed_at":"2030-01-01T00:00:00Z"}`,
		`{"AUTO_PURCHASE_CHANGED_BY":"mallory","Auto_Purchase_Changed_Via":"x","Auto_Purchase_Changed_At":"2030-01-01T00:00:00Z"}`,
		`{"AutoPurchaseChangedBy":"mallory","AutoPurchaseChangedVia":"x","AutoPurchaseChangedAt":"2030-01-01T00:00:00Z"}`,
		`{"autopurchasechangedby":"mallory"}`,
	} {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, stored, body)
		require.NoError(t, err, body)
		require.NotNil(t, saved, body)
		require.NotNil(t, saved.AutoPurchaseChangedBy, body)
		assert.Equal(t, "real-actor", *saved.AutoPurchaseChangedBy, body)
		assert.Equal(t, "user", *saved.AutoPurchaseChangedVia, body)
		assert.True(t, saved.AutoPurchaseChangedAt.Equal(at), body)
	}
}
