package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// shortFinalizeBackoffs swaps the retry delays for the test's duration.
func shortFinalizeBackoffs(t *testing.T, d ...time.Duration) {
	t.Helper()
	orig := finalizeRevocationBackoffs
	finalizeRevocationBackoffs = d
	t.Cleanup(func() { finalizeRevocationBackoffs = orig })
}

func inFlightRows(ids ...string) []*config.PurchaseHistoryRecord {
	rows := make([]*config.PurchaseHistoryRecord, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, &config.PurchaseHistoryRecord{PurchaseID: id})
	}
	return rows
}

func expectMark(store *MockConfigStore, id string, err error) *mock.Call {
	return store.On("MarkPurchaseRevoked", mock.Anything, id, mock.Anything, "direct-api", "", (*float64)(nil), "").
		Return(err).Once()
}

// One row finalizes first try, one on a retry, one never: the counts must say
// exactly that, and every row must be stamped with the same sweep timestamp.
func TestFinalizeInFlightRevocations_Accounting(t *testing.T) {
	shortFinalizeBackoffs(t, time.Millisecond, time.Millisecond)
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetPurchaseHistoryInFlight", ctx).Return(inFlightRows("first", "retry", "never"), nil)
	dbDown := errors.New("db down")
	expectMark(store, "first", nil)
	expectMark(store, "retry", dbDown)
	expectMark(store, "retry", nil)
	expectMark(store, "never", dbDown)
	expectMark(store, "never", dbDown)
	expectMark(store, "never", dbDown)
	m := &Manager{config: store}

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, FinalizeResult{Found: 3, Finalized: 2, Errored: 1}, *res)
	store.AssertExpectations(t)
	store.AssertNumberOfCalls(t, "MarkPurchaseRevoked", 6)

	var stamps []time.Time
	for _, c := range store.Calls {
		if c.Method == "MarkPurchaseRevoked" {
			stamps = append(stamps, c.Arguments.Get(2).(time.Time))
		}
	}
	for _, ts := range stamps {
		assert.True(t, ts.Equal(stamps[0]), "every row in one sweep shares one revokedAt")
	}
}

func TestFinalizeInFlightRevocations_ListErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetPurchaseHistoryInFlight", ctx).Return(nil, errors.New("list failed"))
	m := &Manager{config: store}

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.EqualError(t, err, "list failed")
	assert.Nil(t, res)
	store.AssertNotCalled(t, "MarkPurchaseRevoked", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// A context canceled during a backoff stops the sweep promptly: the row being
// retried counts as Errored, later rows are not attempted, and the partial
// counts come back with an error wrapping context.Canceled.
func TestFinalizeInFlightRevocations_CanceledDuringBackoffStopsSweep(t *testing.T) {
	shortFinalizeBackoffs(t, 3*time.Second, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := new(MockConfigStore)
	store.On("GetPurchaseHistoryInFlight", ctx).Return(inFlightRows("ok", "stuck", "later"), nil)
	expectMark(store, "ok", nil)
	expectMark(store, "stuck", errors.New("db down")).Run(func(mock.Arguments) { cancel() })
	m := &Manager{config: store}

	start := time.Now()
	res, err := m.FinalizeInFlightRevocations(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	assert.Equal(t, FinalizeResult{Found: 3, Finalized: 1, Errored: 1}, *res)
	assert.Less(t, elapsed, time.Second, "the sweep must not sleep through the backoff after cancellation")
	store.AssertNumberOfCalls(t, "MarkPurchaseRevoked", 2)
}

// Issue #1523: before the finalize pass the sweep releases flags that stayed
// set past staleRevocationInFlightAfter with no recorded Return. Released rows
// are counted but never marked revoked.
func TestFinalizeInFlightRevocations_ReleasesStaleFlagsWithoutMarking(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("ReleaseStaleRevocationsInFlight", ctx, staleRevocationInFlightAfter).
		Return([]string{"crashed-before-return", "legacy"}, nil).Once()
	store.On("GetPurchaseHistoryInFlight", ctx).Return(inFlightRows("returned"), nil).Once()
	expectMark(store, "returned", nil)
	m := &Manager{config: store}

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, FinalizeResult{Found: 1, Finalized: 1, Released: 2}, *res)
	store.AssertExpectations(t)
	store.AssertNumberOfCalls(t, "MarkPurchaseRevoked", 1)
}

// A release failure must not stop the finalize pass (the two queues are
// disjoint), but the tick must still fail so the outage is visible.
func TestFinalizeInFlightRevocations_ReleaseErrorStillFinalizes(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("ReleaseStaleRevocationsInFlight", ctx, staleRevocationInFlightAfter).
		Return(nil, errors.New("release failed")).Once()
	store.On("GetPurchaseHistoryInFlight", ctx).Return(inFlightRows("returned"), nil).Once()
	expectMark(store, "returned", nil)
	m := &Manager{config: store}

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.ErrorContains(t, err, "release failed")
	require.NotNil(t, res)
	assert.Equal(t, FinalizeResult{Found: 1, Finalized: 1}, *res)
	store.AssertExpectations(t)
}

// Both a release failure and a list failure are reported.
func TestFinalizeInFlightRevocations_ReleaseAndListErrorsJoined(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("ReleaseStaleRevocationsInFlight", ctx, staleRevocationInFlightAfter).
		Return(nil, errors.New("release failed")).Once()
	store.On("GetPurchaseHistoryInFlight", ctx).Return(nil, errors.New("list failed")).Once()
	m := &Manager{config: store}

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.ErrorContains(t, err, "release failed")
	require.ErrorContains(t, err, "list failed")
	assert.Nil(t, res)
}

// The timeout should outlast a normal request still between its flip and its
// Return. Cloud Run's request timeout caps at an hour; Fargate and Container
// Apps have no cap, and correctness does not depend on this (issue #1525).
func TestStaleRevocationInFlightAfter_OutlastsLongestRequest(t *testing.T) {
	assert.Greater(t, staleRevocationInFlightAfter, time.Hour)
}
