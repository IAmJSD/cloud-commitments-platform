//go:build integration

package config

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestManagedRootCapabilityRefusesUnmanaged(t *testing.T) {
	ctx := t.Context()
	f := newRampFixture(ctx, t, 0, 0, 3)
	capability, ok := any(f.store).(interface {
		ResolveManagedPlanRootTx(context.Context, pgx.Tx, *PurchasePlan) (*PurchaseExecution, error)
	})
	if !ok {
		t.Skip("parent lacks the capability; behavioral parent proof uses the actual Manager")
	}
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		p, err := f.store.LockPurchasePlanTx(ctx, tx, f.planID)
		require.NoError(t, err)
		_, err = capability.ResolveManagedPlanRootTx(ctx, tx, p)
		require.Error(t, err)
		return nil
	}))
}
