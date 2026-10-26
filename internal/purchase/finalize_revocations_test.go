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
