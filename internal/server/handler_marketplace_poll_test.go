package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeMarketplacePoller struct {
	result *api.MarketplacePollResult
	err    error
	calls  int
}

func (f *fakeMarketplacePoller) PollMarketplaceListings(context.Context) (*api.MarketplacePollResult, error) {
	f.calls++
	return f.result, f.err
}

func finalizeApp(revocations *purchase.FinalizeResult, revErr error, poller *fakeMarketplacePoller) (*Application, *int) {
	revCalls := 0
	p := &testutil.MockPurchaseManager{}
	p.FinalizeInFlightRevocationsFunc = func(context.Context) (*purchase.FinalizeResult, error) {
		revCalls++
		return revocations, revErr
	}
	return &Application{Scheduler: &testutil.MockScheduler{}, Purchase: p, marketplacePoller: poller}, &revCalls
}

// Issue #27: the finalize_revocations sweep also polls RI Marketplace
// listings, and its result keeps the revocation counts under their old keys.
func TestFinalizeSweep_PollsMarketplaceListings(t *testing.T) {
	poller := &fakeMarketplacePoller{result: &api.MarketplacePollResult{Found: 3, Active: 1, Settled: 2}}
	app, revCalls := finalizeApp(&purchase.FinalizeResult{Found: 1, Finalized: 1}, nil, poller)

	out, err := app.HandleScheduledTask(testutil.TestContext(t), TaskFinalizeRevocations, ScheduledTaskParams{})

	require.NoError(t, err)
	assert.Equal(t, 1, *revCalls)
	assert.Equal(t, 1, poller.calls)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Found":1,"Finalized":1,"Errored":0,"Released":0,
		"marketplace_listings":{"found":3,"active":1,"settled":2,"unpriced":0,"skipped":0,"errored":0}}`, string(encoded))
}

// A failing revocation repair does not skip the poll, and still fails the task.
func TestFinalizeSweep_RevocationFailureStillPolls(t *testing.T) {
	poller := &fakeMarketplacePoller{result: &api.MarketplacePollResult{}}
	app, _ := finalizeApp(nil, errors.New("db down"), poller)

	_, err := app.HandleScheduledTask(testutil.TestContext(t), TaskFinalizeRevocations, ScheduledTaskParams{})

	require.ErrorContains(t, err, "db down")
	assert.Equal(t, 1, poller.calls)
}

// A failing poll fails the task without hiding the revocation repair.
func TestFinalizeSweep_PollFailureFailsTheTask(t *testing.T) {
	poller := &fakeMarketplacePoller{err: errors.New("list failed")}
	app, revCalls := finalizeApp(&purchase.FinalizeResult{Found: 2, Finalized: 2}, nil, poller)

	out, err := app.HandleScheduledTask(testutil.TestContext(t), TaskFinalizeRevocations, ScheduledTaskParams{})

	require.ErrorContains(t, err, "marketplace listing poll: list failed")
	assert.Equal(t, 1, *revCalls)
	res, ok := out.(*finalizeSweepResult)
	require.True(t, ok)
	assert.Equal(t, 2, res.Finalized)
}

// Without an API handler (no database wired yet) the poll is a no-op.
func TestFinalizeSweep_NoPollerIsANoOp(t *testing.T) {
	app, _ := finalizeApp(&purchase.FinalizeResult{}, nil, nil)
	app.marketplacePoller = nil

	out, err := app.HandleScheduledTask(testutil.TestContext(t), TaskFinalizeRevocations, ScheduledTaskParams{})

	require.NoError(t, err)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "marketplace_listings")
}
