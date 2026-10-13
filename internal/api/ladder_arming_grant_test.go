package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Coverage for the ladder arming gate (platform#118 PR2): turning
// LadderExecutionEnabled on, or arming / raising a ladder_config, needs
// execute:purchases for a real user. Admin without the verb and the admin API
// key are refused; de-escalations stay ordinary. Reuses the apg* helpers of
// auto_purchase_grant_test.go.

const lgAcct = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

func lgStored(enabled bool, mode string, capUSD *float64) *config.LadderConfigDB {
	return &config.LadderConfigDB{ID: "cfg-1", CloudAccountID: lgAcct, Provider: "aws", Enabled: enabled, Mode: mode, MaxHourlyCommitPerRun: capUSD}
}

func lgBody(enabled bool, mode, capJSON string) string {
	capField := ""
	if capJSON != "" {
		capField = `"max_hourly_commit_per_run":` + capJSON + `,`
	}
	return fmt.Sprintf(`{"cloud_account_id":"%s","provider":"aws","enabled":%t,"mode":"%s","cadence":"daily",%s%s}`,
		lgAcct, enabled, mode, capField, ladderValidRamp)
}

func lgCall(caller apgCaller, stored *config.LadderConfigDB, body string) (*MockConfigStore, error) {
	store := new(MockConfigStore)
	store.On("GetLadderConfig", mock.Anything, lgAcct, "aws").Return(stored, nil)
	store.On("UpsertLadderConfig", mock.Anything, mock.Anything).Return(&config.LadderConfigDB{ID: "cfg-1"}, nil).Maybe()
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Provider: "aws"}, nil
	}
	h, req := caller.handlerAndReq(store, body)
	_, err := h.upsertLadderConfig(context.Background(), req)
	return store, err
}

func TestLadderArmingGate_ConfigWrites(t *testing.T) {
	admin := apgCaller{perms: apgAdminOnly}
	granted := apgCaller{perms: apgAdminPlus}
	key := apgCaller{key: true}
	auto, email := "auto_approve", "email_approval"
	cap1, cap03 := apgCap(1), apgCap(0.3)

	tests := []struct {
		name        string
		caller      apgCaller
		stored      *config.LadderConfigDB
		body        string
		wantRefused bool
	}{
		{"create armed: admin refused", admin, nil, lgBody(true, auto, "1"), true},
		{"create armed: admin API key refused", key, nil, lgBody(true, auto, "1"), true},
		{"create armed: execute:purchases allowed", granted, nil, lgBody(true, auto, "1"), false},
		{"create disabled auto: admin allowed", admin, nil, lgBody(false, auto, "1"), false},
		{"create enabled email_approval: admin allowed", admin, nil, lgBody(true, email, ""), false},
		{"enable a disabled auto config: admin refused", admin, lgStored(false, auto, cap1), lgBody(true, auto, "1"), true},
		{"enable a disabled auto config: admin API key refused", key, lgStored(false, auto, cap1), lgBody(true, auto, "1"), true},
		{"email_approval to auto: admin refused", admin, lgStored(true, email, cap1), lgBody(true, auto, "1"), true},
		{"email_approval to auto: execute:purchases allowed", granted, lgStored(true, email, cap1), lgBody(true, auto, "1"), false},
		{"raise cap: admin refused", admin, lgStored(true, auto, cap1), lgBody(true, auto, "2"), true},
		{"raise cap: admin API key refused", key, lgStored(true, auto, cap1), lgBody(true, auto, "2"), true},
		{"raise cap: execute:purchases allowed", granted, lgStored(true, auto, cap1), lgBody(true, auto, "2"), false},
		{"raise by one micro: admin refused", admin, lgStored(true, auto, cap1), lgBody(true, auto, "1.000001"), true},
		{"clear cap: admin refused", admin, lgStored(true, auto, cap1), lgBody(true, auto, ""), true},
		{"clear cap: admin API key refused", key, lgStored(true, auto, cap1), lgBody(true, auto, ""), true},
		{"same cap: admin allowed", admin, lgStored(true, auto, cap1), lgBody(true, auto, "1"), false},
		{"same cap 0.3 (binary-float trap): admin allowed", admin, lgStored(true, auto, cap03), lgBody(true, auto, "0.30"), false},
		{"lower cap: admin API key allowed", key, lgStored(true, auto, cap1), lgBody(true, auto, "0.5"), false},
		{"set a cap on an uncapped armed config: admin API key allowed", key, lgStored(true, auto, nil), lgBody(true, auto, "5"), false},
		{"uncapped stays uncapped: admin allowed", admin, lgStored(true, auto, nil), lgBody(true, auto, ""), false},
		{"disarm (disable): admin API key allowed", key, lgStored(true, auto, cap1), lgBody(false, auto, "1"), false},
		{"switch to email_approval: admin API key allowed", key, lgStored(true, auto, cap1), lgBody(true, email, "1"), false},
		{"raise cap while disabled: admin allowed", admin, lgStored(false, auto, cap1), lgBody(false, auto, "9"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := lgCall(tc.caller, tc.stored, tc.body)
			if tc.wantRefused {
				apgRefused(t, err)
				assert.False(t, apgCalled(store, "UpsertLadderConfig"), "a refused write must not reach the store")
				return
			}
			require.NoError(t, err)
			assert.True(t, apgCalled(store, "UpsertLadderConfig"))
		})
	}
}

func TestLadderArmingGate_ConfigLookupErrorFailsClosed(t *testing.T) {
	store := new(MockConfigStore)
	store.On("GetLadderConfig", mock.Anything, lgAcct, "aws").Return((*config.LadderConfigDB)(nil), fmt.Errorf("db down"))
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Provider: "aws"}, nil
	}
	h, req := apgCaller{perms: apgAdminPlus}.handlerAndReq(store, lgBody(true, "auto_approve", "1"))
	_, err := h.upsertLadderConfig(context.Background(), req)
	require.Error(t, err)
	assert.False(t, apgCalled(store, "UpsertLadderConfig"))
}

func TestLadderArmingGate_CapValidation(t *testing.T) {
	for _, capJSON := range []string{"0", "-1", "0.0000004", "1.1234567", "1e13", "1e308", `"5"`} {
		t.Run(capJSON, func(t *testing.T) {
			store, err := lgCall(apgCaller{perms: apgAdminPlus}, nil, lgBody(false, "auto_approve", capJSON))
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 400, ce.code)
			assert.False(t, apgCalled(store, "UpsertLadderConfig"))
		})
	}
	// Boundary values stay valid.
	for _, capJSON := range []string{"0.000001", "123.456789", "1e12"} {
		_, err := lgCall(apgCaller{perms: apgAdminPlus}, nil, lgBody(false, "auto_approve", capJSON))
		require.NoError(t, err, capJSON)
	}
}

func TestLadderArmingGate_ExecutionSwitch(t *testing.T) {
	off, on := baseValidConfig(), baseValidConfig()
	on.LadderExecutionEnabled = true
	const flip = `{"ladder_execution_enabled":true}`

	t.Run("off to on: admin without grant refused", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, off, flip)
		apgRefused(t, err)
		assert.Nil(t, saved)
	})
	t.Run("off to on: admin API key refused", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{key: true}, off, flip)
		apgRefused(t, err)
		assert.Nil(t, saved)
	})
	t.Run("off to on: execute:purchases allowed and stamped", func(t *testing.T) {
		before := time.Now().UTC().Add(-time.Second)
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminPlus}, off, flip)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.True(t, saved.LadderExecutionEnabled)
		require.NotNil(t, saved.LadderExecutionChangedBy)
		assert.Equal(t, apgUserID, *saved.LadderExecutionChangedBy)
		require.NotNil(t, saved.LadderExecutionChangedVia)
		assert.Equal(t, string(PrincipalSession), *saved.LadderExecutionChangedVia)
		require.NotNil(t, saved.LadderExecutionChangedAt)
		assert.True(t, saved.LadderExecutionChangedAt.After(before))
		assert.Nil(t, saved.AutoPurchaseChangedBy, "the auto-purchase stamp is untouched")
	})
	t.Run("on to off: admin API key allowed and stamped", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{key: true}, on, `{"ladder_execution_enabled":false}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.False(t, saved.LadderExecutionEnabled)
		require.NotNil(t, saved.LadderExecutionChangedVia)
		assert.Equal(t, string(PrincipalAdminAPIKey), *saved.LadderExecutionChangedVia)
	})
	t.Run("unchanged switch: admin may save other settings, no stamp", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, on, `{"ladder_execution_enabled":true,"default_coverage":70}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.Nil(t, saved.LadderExecutionChangedBy)
	})
	t.Run("laddering_enabled (planning only) needs no grant", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{key: true}, off, `{"laddering_enabled":true}`)
		require.NoError(t, err)
		require.NotNil(t, saved)
		assert.True(t, saved.LadderingEnabled)
		assert.Nil(t, saved.LadderExecutionChangedBy)
	})
	t.Run("both switches flipped without grant: refused", func(t *testing.T) {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, off, `{"ladder_execution_enabled":true,"auto_purchase_enabled":false}`)
		apgRefused(t, err)
		assert.Nil(t, saved)
	})
}

func TestLadderArmingGate_StampsCannotBeForgedViaConfigBody(t *testing.T) {
	stored := baseValidConfig()
	by, via := "real-actor", "user"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stored.LadderExecutionChangedBy, stored.LadderExecutionChangedVia, stored.LadderExecutionChangedAt = &by, &via, &at

	for _, body := range []string{
		`{"ladder_execution_changed_by":"mallory","ladder_execution_changed_via":"x","ladder_execution_changed_at":"2030-01-01T00:00:00Z"}`,
		`{"LadderExecutionChangedBy":"mallory","LadderExecutionChangedVia":"x","LadderExecutionChangedAt":"2030-01-01T00:00:00Z"}`,
	} {
		saved, err := apgConfigCall(t, apgCaller{perms: apgAdminOnly}, stored, body)
		require.NoError(t, err, body)
		require.NotNil(t, saved, body)
		require.NotNil(t, saved.LadderExecutionChangedBy, body)
		assert.Equal(t, "real-actor", *saved.LadderExecutionChangedBy, body)
		assert.True(t, saved.LadderExecutionChangedAt.Equal(at), body)
	}
}
