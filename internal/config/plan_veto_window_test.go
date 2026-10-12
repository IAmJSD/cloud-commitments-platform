package config

import (
	"context"
	"testing"

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
