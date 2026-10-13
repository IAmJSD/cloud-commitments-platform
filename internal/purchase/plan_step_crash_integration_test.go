//go:build integration

package purchase

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S11 crash points on real PostgreSQL. The resolver and email sender are
// fixtures; the store is real with one injected failure each. The two other
// points of the table are covered elsewhere: "after commit, before email" by
// TestAttachIntegration_EmailFailureLeavesPendingAndNextTickDoesNotResolveAgain,
// and "after the provider call" by TestRedriveReusesDerivedTokensForEveryProvider.

type suppressionFailingStore struct{ *config.PostgresStore }

func (suppressionFailingStore) CreateSuppressionTx(context.Context, pgx.Tx, *config.PurchaseSuppression) error {
	return errors.New("injected crash while writing suppressions")
}

// rotateLosingStore drops the first token rotation, as a crash between the
// email going out and the notified promotion would.
type rotateLosingStore struct {
	*config.PostgresStore
	rotations atomic.Int32
}

func (s *rotateLosingStore) RotatePendingApprovalToken(ctx context.Context, id, hash string, exp time.Time) (bool, error) {
	if s.rotations.Add(1) == 1 {
		return false, errors.New("injected crash after the email")
	}
	return s.PostgresStore.RotatePendingApprovalToken(ctx, id, hash, exp)
}

func TestCrash_BeforeCommitPersistsNothing(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "crash-before-commit", time.Now().Add(24*time.Hour))
	sender := newCountingSender(ctx)
	m := &Manager{config: suppressionFailingStore{store}, email: sender, resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 3)}}}

	require.False(t, m.sendPlanNotification(ctx, plan))

	assert.Empty(t, stepRowsInDB(ctx, t, store, plan.ID), "the row insert rolled back with the failed suppressions")
	assert.Zero(t, suppressedTotal(ctx, t, pool))
	assert.Zero(t, sender.sends)
}

func TestCrash_AfterEmailBeforeNotifiedResendsAndPromotes(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "crash-after-email", time.Now().Add(24*time.Hour))
	sender := newCountingSender(ctx)
	res := &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 3)}}
	m := &Manager{config: &rotateLosingStore{PostgresStore: store}, email: sender, resolver: res}

	require.False(t, m.sendPlanNotification(ctx, plan))
	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "pending", rows[0].Status, "the scheduler refuses a pending row")
	assert.Equal(t, 1, sender.sends)

	require.True(t, m.sendPlanNotification(ctx, plan), "the next tick re-sends and promotes")
	rows = stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "notified", rows[0].Status)
	assert.Equal(t, 2, sender.sends)
	assert.EqualValues(t, 1, res.calls.Load(), "no second resolve")
}
