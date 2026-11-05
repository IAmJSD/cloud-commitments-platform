package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireVetoWindow(t *testing.T) {
	for days, wantErr := range map[int]bool{-1: true, 0: true, 1: false, 7: false} {
		err := (&PurchasePlan{ID: "p", NotificationDaysBefore: days}).RequireVetoWindow()
		if wantErr {
			assert.ErrorIs(t, err, ErrNoVetoWindow, "days=%d", days)
		} else {
			assert.NoError(t, err, "days=%d", days)
		}
	}
}

func TestVetoWindowEnd_FixedDaysInAnyLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	// 2026-03-07 12:00 EST, 3 days later crosses the US spring-forward on 03-08.
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	plan := &PurchasePlan{NotificationDaysBefore: 3}
	got := plan.VetoWindowEnd(now)
	assert.True(t, now.Add(72*time.Hour).Equal(got), "three fixed 24h days, not wall-clock days")
	assert.Equal(t, time.UTC, got.Location())
	assert.True(t, (&PurchasePlan{}).VetoWindowEnd(now).Equal(now), "zero days is now")
}

func TestPlanStepLockKeys_SortedDedupedValidated(t *testing.T) {
	keys, err := planStepLockKeys([]string{"gcp", "aws", "azure", "aws"})
	require.NoError(t, err)
	assert.Equal(t, []string{"cudly:planstep:aws", "cudly:planstep:azure", "cudly:planstep:gcp"}, keys)

	_, err = planStepLockKeys(nil)
	assert.Error(t, err)
	_, err = planStepLockKeys([]string{"aws", ""})
	assert.Error(t, err)
	assert.Error(t, (&PostgresStore{}).LockPlanProvidersTx(context.Background(), nil, nil))
}
