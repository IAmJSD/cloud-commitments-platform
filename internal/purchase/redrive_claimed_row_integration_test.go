//go:build integration

package purchase

// Issue #1037 on real PostgreSQL: a write lands between the stranded-approval
// sweep's SELECT and its approved -> running claim. The re-drive must buy the
// row the claim returned. Real store, real Manager, counting fake provider
// (labeled fixture; nothing here reaches a cloud API).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// editAfterListStore runs the real stale-approved SELECT, then holds every
// caller until afterList has run once all n sweeps have listed, so the edit
// lands after each sweep's snapshot and before any claim.
//
// The SELECT is called with a zero age instead of staleApprovedThreshold: the
// fixture row was written moments ago, and the 15-minute age filter is not
// what this test is about. Every other store call is the production one.
type editAfterListStore struct {
	*config.PostgresStore
	listed    sync.WaitGroup
	edited    chan struct{}
	afterList func() error
	editErr   error
	once      sync.Once
}

func newEditAfterListStore(store *config.PostgresStore, sweeps int, afterList func() error) *editAfterListStore {
	s := &editAfterListStore{PostgresStore: store, edited: make(chan struct{}), afterList: afterList}
	s.listed.Add(sweeps)
	return s
}

func (s *editAfterListStore) GetStaleApprovedExecutions(ctx context.Context, _ time.Duration) ([]config.PurchaseExecution, error) {
	rows, err := s.PostgresStore.GetStaleApprovedExecutions(ctx, 0)
	s.listed.Done()
	s.once.Do(func() {
		go func() {
			s.listed.Wait()
			s.editErr = s.afterList()
			close(s.edited)
		}()
	})
	<-s.edited
	return rows, err
}

// editRow rewrites the stored row's recommendations through the production
// full-row upsert, the same write an edit or a concurrent writer performs. It
// runs off the test goroutine, so it returns its error for the test to check.
func editRow(ctx context.Context, store *config.PostgresStore, id string, mutate func(*config.RecommendationRecord)) error {
	row, err := store.GetExecutionByID(ctx, id)
	if err != nil {
		return err
	}
	if row.Status != "approved" {
		return fmt.Errorf("row %s is %q before the edit, want approved", id, row.Status)
	}
	mutate(&row.Recommendations[0])
	row.TotalUpfrontCost = row.Recommendations[0].UpfrontCost
	return store.SavePurchaseExecution(ctx, row)
}

func (f *rampStepFixture) boughtCounts() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.counts...)
}

// The row is repriced from 1 to 5 instances after four concurrent sweeps have
// listed it. Exactly one sweep wins the claim and buys the claimed 5; before
// the fix the winner bought the listed 1 and saved that snapshot back.
func TestRecoverStrandedApprovals_ConcurrentSweepsBuyClaimedRowOnce(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	row := claimRaceRow(ctx, t, f, "approved")
	require.Equal(t, 1, row.Recommendations[0].Count)

	const sweeps = 4
	store := newEditAfterListStore(f.store, sweeps, func() error {
		return editRow(ctx, f.store, row.ExecutionID, func(r *config.RecommendationRecord) {
			r.Count = 5
			r.UpfrontCost = 1500
		})
	})
	f.manager.config = store

	var wg sync.WaitGroup
	recovered := make([]int, sweeps)
	errs := make([]error, sweeps)
	for i := 0; i < sweeps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recovered[i], errs[i] = f.manager.RecoverStrandedApprovals(ctx)
		}()
	}
	wg.Wait()
	require.NoError(t, store.editErr)

	total := 0
	for i := range recovered {
		require.NoError(t, errs[i])
		total += recovered[i]
	}
	assert.Equal(t, 1, total, "exactly one sweep wins the claim")
	assert.Equal(t, []int{5}, f.boughtCounts(), "one provider call, for the claimed row's 5 instances")
	got := rowStatus(ctx, t, f, row.ExecutionID)
	assert.Equal(t, "completed", got.Status)
	require.Len(t, got.Recommendations, 1)
	assert.Equal(t, 5, got.Recommendations[0].Count, "the terminal save must not write the listed snapshot back")
	assert.InDelta(t, 1500, got.TotalUpfrontCost, 0.001)
}

// The listed row was a safe RDS re-drive; the edit turns it into EC2, which has
// no provider duplicate guard. Nothing may be bought: the claimed row is failed
// as the safe-fail path would fail it, without the no_submission stamp.
func TestRecoverStrandedApprovals_ClaimedRowTurnedUnsafeIsFailedNotBought(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	row := claimRaceRow(ctx, t, f, "approved")

	store := newEditAfterListStore(f.store, 1, func() error {
		return editRow(ctx, f.store, row.ExecutionID, func(r *config.RecommendationRecord) {
			r.Service = "ec2"
			r.ResourceType = "m5.large"
		})
	})
	f.manager.config = store

	recovered, err := f.manager.RecoverStrandedApprovals(ctx)
	require.NoError(t, store.editErr)

	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	assert.Equal(t, 0, f.purchaseCount(), "neither the listed RDS rec nor the claimed EC2 rec may be bought")
	got := rowStatus(ctx, t, f, row.ExecutionID)
	assert.Equal(t, "failed", got.Status)
	assert.False(t, got.NoSubmission, "the interrupted original run may have bought: no_submission must stay false")
	assert.Contains(t, got.Error, "start a new purchase")
	require.Len(t, got.Recommendations, 1)
	assert.Equal(t, "ec2", got.Recommendations[0].Service, "the failed row keeps the claimed recommendations")
}
