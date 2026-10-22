package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
)

// Coverage for issue #1196: PUT /api/config must validate ri_exchange_mode the
// same way PUT /api/ri-exchange/config does (exactly "manual" or "auto").
//
// The principal below holds execute:ri-exchange, so the #1765 auto-mode gate
// never refuses these writes. Without the check, every invalid mode is
// therefore stored as-is, which is what makes each refusal case fail before the
// fix rather than passing on a 403 from the gate.

const riExchangeModeErr = "ri_exchange_mode must be 'manual' or 'auto'"

// savedGlobalConfig returns the config passed to the last SaveGlobalConfig call.
func savedGlobalConfig(t *testing.T, m *mocks.MockConfigStore) config.GlobalConfig {
	t.Helper()
	for i := len(m.Calls) - 1; i >= 0; i-- {
		if m.Calls[i].Method == "SaveGlobalConfig" {
			cfg, ok := m.Calls[i].Arguments.Get(1).(*config.GlobalConfig)
			require.True(t, ok, "SaveGlobalConfig argument must be a *config.GlobalConfig")
			return *cfg
		}
	}
	t.Fatal("SaveGlobalConfig was never called")
	return config.GlobalConfig{}
}

// mustJSONString encodes s as a JSON string literal.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

func assertModeRejected(t *testing.T, err error, store *mocks.MockConfigStore) {
	t.Helper()
	require.Error(t, err, "an invalid ri_exchange_mode must be refused")
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), riExchangeModeErr)
	assert.False(t, wrote(store), "a rejected write must not reach the store")
}

func TestUpdateConfig_RejectsInvalidRIExchangeMode(t *testing.T) {
	// Typos, the empty string, wrong case and padded values. The consumer
	// matches the two valid values exactly, so none of these is "close enough".
	for _, mode := range []string{"Auto", "AUTO", "Manual", "", "atuo", "automatic", "enabled", " auto", "manual "} {
		t.Run("mode="+mode, func(t *testing.T) {
			r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, disarmedConfig())

			_, err := r.Route(context.Background(), "PUT", genericPath,
				autoGateTokenReq(`{"ri_exchange_mode":`+mustJSONString(t, mode)+`}`))

			assertModeRejected(t, err, store)
		})
	}
}

// TestUpdateConfig_InvalidRIExchangeModeRejectedBeforeGate pins the order: a
// malformed write is a 400, never a 403 that reveals the caller's grant state.
func TestUpdateConfig_InvalidRIExchangeModeRejectedBeforeGate(t *testing.T) {
	r, store := autoGateSessionRouter(t, autoGateAdminOnly, disarmedConfig())

	_, err := r.Route(context.Background(), "PUT", genericPath,
		autoGateTokenReq(genericBody("Auto", true, 100, 500)))

	assertModeRejected(t, err, store)
}

func TestUpdateConfig_AcceptsValidRIExchangeMode(t *testing.T) {
	for _, mode := range []string{"manual", "auto"} {
		t.Run("mode="+mode, func(t *testing.T) {
			r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, disarmedConfig())

			_, err := r.Route(context.Background(), "PUT", genericPath,
				autoGateTokenReq(`{"ri_exchange_mode":`+mustJSONString(t, mode)+`}`))

			require.NoError(t, err)
			assert.Equal(t, mode, savedGlobalConfig(t, store).RIExchangeMode)
		})
	}
}

// legacyModeConfig is a row stored before this validation existed, carrying a
// mode the API no longer accepts.
func legacyModeConfig(mode string) config.GlobalConfig {
	cfg := disarmedConfig()
	cfg.RIExchangeMode = mode
	return cfg
}

// TestUpdateConfig_LegacyStoredModeDoesNotBlockUnrelatedWrites pins that the
// check applies to the value the caller sends, not to whatever is already
// stored. Otherwise a single legacy row would 400 every partial settings save,
// including the laddering kill-switch toggle.
func TestUpdateConfig_LegacyStoredModeDoesNotBlockUnrelatedWrites(t *testing.T) {
	for name, body := range map[string]string{
		"unrelated field":      `{"laddering_enabled":true}`,
		"explicit JSON null":   `{"ri_exchange_mode":null,"laddering_enabled":true}`,
		"null with whitespace": `{"ri_exchange_mode" :  null ,"laddering_enabled":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, legacyModeConfig("automatic"))

			_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(body))

			require.NoError(t, err, "a write that does not set ri_exchange_mode must not be blocked by a legacy row")
			saved := savedGlobalConfig(t, store)
			assert.True(t, saved.LadderingEnabled, "the field the body carried must be applied")
			assert.Equal(t, "automatic", saved.RIExchangeMode, "the legacy mode is preserved, not silently rewritten")
		})
	}
}

// TestUpdateConfig_LegacyStoredModeCanBeRepaired pins the repair path for a
// legacy row: sending a valid mode replaces it.
func TestUpdateConfig_LegacyStoredModeCanBeRepaired(t *testing.T) {
	r, store := autoGateSessionRouter(t, autoGateAdminOnly, legacyModeConfig("Auto"))

	_, err := r.Route(context.Background(), "PUT", genericPath,
		autoGateTokenReq(`{"ri_exchange_mode":"manual"}`))

	require.NoError(t, err)
	assert.Equal(t, "manual", savedGlobalConfig(t, store).RIExchangeMode)
}

// TestUpdateConfig_LegacyStoredModeResentIsRejected pins that a caller cannot
// re-assert a legacy value just because it is already stored.
func TestUpdateConfig_LegacyStoredModeResentIsRejected(t *testing.T) {
	r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, legacyModeConfig("Auto"))

	_, err := r.Route(context.Background(), "PUT", genericPath,
		autoGateTokenReq(`{"ri_exchange_mode":"Auto"}`))

	assertModeRejected(t, err, store)
}
