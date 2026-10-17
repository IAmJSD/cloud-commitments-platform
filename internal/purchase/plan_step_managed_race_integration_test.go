//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// W15: the notifier is a new-root inserter. It judges the marker on the plan
// row it locked, never on the copy the tick listed, so a plan the command
// converted in between gets no phantom root.

type claimRace struct {
	store  *config.PostgresStore
	pool   *pgxpool.Pool
	plan   *config.PurchasePlan
	sender *countingSender
	m      *Manager
}

func newClaimRace(ctx context.Context, t *testing.T) *claimRace {
	store, pool := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "claim-race", time.Now().Add(24*time.Hour))
	sender := newCountingSender(ctx)
	res := &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 2)}}
	return &claimRace{store, pool, plan, sender, &Manager{config: store, email: sender, resolver: res}}
}

func TestClaimSkipsPlanTheCommandConvertedWhileItWaited(t *testing.T) {
	ctx := context.Background()
	r := newClaimRace(ctx, t)
	command := testhelpers.HoldCommandFootprint(ctx, t, r.pool, r.plan.ID)
	done := make(chan bool, 1)
	go func() { done <- r.m.sendPlanNotification(ctx, r.plan) }() // r.plan is the stale, unmanaged copy
	testhelpers.WaitForLockWait(ctx, t, r.pool, "%FOR UPDATE%")
	command.SetManaged(ctx, t, r.plan.ID)
	require.NoError(t, command.Commit(ctx))

	require.False(t, testhelpers.Await(t, done))
	require.Empty(t, stepRowsInDB(ctx, t, r.store, r.plan.ID), "no phantom root on the converted plan")
	require.Zero(t, r.sender.sends)
}

func TestCommandCaptureSeesRootTheClaimCommittedFirst(t *testing.T) {
	ctx := context.Background()
	r := newClaimRace(ctx, t)
	require.True(t, r.m.sendPlanNotification(ctx, r.plan))
	rows := stepRowsInDB(ctx, t, r.store, r.plan.ID)
	require.Len(t, rows, 1)

	require.Equal(t, []string{rows[0].ExecutionID}, testhelpers.HoldCommandFootprint(ctx, t, r.pool, r.plan.ID).Executions)
}
