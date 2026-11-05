package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

func fireSweepRouter(t *testing.T, off bool) (*Router, func() bool) {
	t.Helper()
	r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, disarmedConfig())
	r.h.fireSweepOff = off
	return r, func() bool { return wrote(store) }
}

func TestUpdateConfig_RefusesDelayWhenNoFireSweep(t *testing.T) {
	r, wrote := fireSweepRouter(t, true)

	_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(`{"purchase_delay_hours": 48}`))

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "purchase_delay_hours")
	assert.False(t, wrote(), "a rejected write must not reach the store")
}

func TestUpdateConfig_AllowsZeroDelayWhenNoFireSweep(t *testing.T) {
	r, _ := fireSweepRouter(t, true)

	_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(`{"purchase_delay_hours": 0}`))

	require.NoError(t, err)
}

func TestUpdateConfig_StoredDelayDoesNotBlockUnrelatedWrites(t *testing.T) {
	for body, name := range map[string]string{
		`{"default_term": 3}`:            "partial save",
		`{"purchase_delay_hours": null}`: "null delay",
		`{"Purchase_Delay_Hours": 0}`:    "mixed-case zero",
	} {
		t.Run(name, func(t *testing.T) {
			stored := disarmedConfig()
			stored.PurchaseDelayHours = 48
			r, store := autoGateSessionRouter(t, autoGateAdminPlusExchange, stored)
			r.h.fireSweepOff = true

			_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(body))

			require.NoError(t, err)
			assert.True(t, wrote(store))
		})
	}
}

func TestUpdateConfig_RefusesMixedCaseDelayKeyWhenNoFireSweep(t *testing.T) {
	r, wrote := fireSweepRouter(t, true)

	_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(`{"Purchase_Delay_Hours": 24}`))

	require.Error(t, err)
	assert.False(t, wrote())
}

func TestUpdateConfig_AllowsDelayWhenFireSweepRuns(t *testing.T) {
	r, _ := fireSweepRouter(t, false)

	_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(`{"purchase_delay_hours": 48}`))

	require.NoError(t, err)
}

func TestUpdateConfig_RefusesDelayHiddenByDuplicateNullKey(t *testing.T) {
	r, wrote := fireSweepRouter(t, true)

	_, err := r.Route(context.Background(), "PUT", genericPath, autoGateTokenReq(`{"purchase_delay_hours": 48, "purchase_delay_hours": null}`))

	require.Error(t, err)
	assert.False(t, wrote(), "a rejected write must not reach the store")
}

func TestPurchaseDelay_RefusesStoredDelayWhenNoFireSweep(t *testing.T) {
	for off, wantErr := range map[bool]bool{true: true, false: false} {
		mockStore := new(MockConfigStore)
		mockStore.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{PurchaseDelayHours: 48}, nil)
		h := &Handler{config: mockStore, fireSweepOff: off}

		delay, err := h.purchaseDelay(context.Background())

		if wantErr {
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, 409, ce.code)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, 48*time.Hour, delay)
	}
}
