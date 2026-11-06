//go:build integration

package purchase

// Real PostgreSQL plus a counting fake provider; nothing reaches a cloud API.
// The tests set the package-level afterRedshiftStampHook, so none is parallel.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type redshiftGateFixture struct {
	*rampStepFixture
	mgr    *Manager
	bought atomic.Int32
	// beforeBuy runs inside each fake provider call.
	beforeBuy func()
}

func newRedshiftGateFixture(ctx context.Context, t *testing.T) *redshiftGateFixture {
	t.Helper()
	f := &redshiftGateFixture{rampStepFixture: newRampStepFixture(ctx, t)}
	f.repairAccounts(ctx, t)
	factory := new(MockProviderFactory)
	provider := new(MockProvider)
	client := new(MockServiceClient)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(provider, nil).Maybe()
	provider.On("GetServiceClient", mock.Anything, mock.Anything, mock.Anything).Return(client, nil).Maybe()
	client.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.AnythingOfType("common.PurchaseOptions")).
		Run(func(mock.Arguments) {
			f.bought.Add(1)
			if f.beforeBuy != nil {
				f.beforeBuy()
			}
		}).
		Return(common.PurchaseResult{Success: true, CommitmentID: "rs-ok"}, nil).Maybe()
	allowFailedNotification(f.email)
	f.mgr = &Manager{config: f.store, email: f.email, providerFactory: factory,
		credStore: awsAccessKeyCredStore(), dashboardURL: "https://dashboard.example.com"}
	t.Cleanup(func() { afterRedshiftStampHook = nil })
	return f
}

// redshiftRow saves a per-account row in its own lineage.
func (f *redshiftGateFixture) redshiftRow(ctx context.Context, t *testing.T, status string) *config.PurchaseExecution {
	t.Helper()
	return f.redshiftRowKeyed(ctx, t, status, uuid.New().String()+":"+f.accounts[0].ID, false)
}

// redshiftRowKeyed saves a row with the given idempotency key; root rows carry
// no cloud_account_id, so their recs fan out per account.
func (f *redshiftGateFixture) redshiftRowKeyed(ctx context.Context, t *testing.T, status, key string, root bool) *config.PurchaseExecution {
	t.Helper()
	acct := f.accounts[0].ID
	rec := config.RecommendationRecord{
		Provider: "aws", Service: "redshift", ResourceType: "ra3.xlplus", Region: "us-east-1",
		Term: 1, Payment: "no-upfront", Count: 2, UpfrontCost: 300, Selected: true, CloudAccountID: &acct,
	}
	row := &config.PurchaseExecution{
		PlanID: f.plan.ID, ExecutionID: uuid.New().String(), IdempotencyKey: key,
		Status: status, StepNumber: 3, ScheduledDate: time.Now(), CloudAccountID: &acct,
		Recommendations: []config.RecommendationRecord{rec},
	}
	if root {
		row.CloudAccountID = nil
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	return row
}

func (f *redshiftGateFixture) stampOf(ctx context.Context, t *testing.T, id string) *time.Time {
	t.Helper()
	var at *time.Time
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT first_dispatch_at FROM purchase_executions WHERE execution_id = $1`, id).Scan(&at))
	return at
}

// A conflicting open lineage refuses the dispatch and takes the stamp with it.
func TestRedshiftDispatchGate_ConflictRollsBackStamp(t *testing.T) {
	ctx := context.Background()
	f := newRedshiftGateFixture(ctx, t)
	open := f.redshiftRow(ctx, t, "running")
	_, err := f.pool.Exec(ctx, `UPDATE purchase_executions SET first_dispatch_at = now() WHERE execution_id = $1`, open.ExecutionID)
	require.NoError(t, err)
	refused := f.redshiftRow(ctx, t, "approved")

	claimed, _ := f.mgr.claimAndExecute(ctx, rowStatusCopy(refused))
	require.True(t, claimed)

	assert.Zero(t, f.bought.Load(), "a refused lineage never reaches the provider")
	got := rowStatus(ctx, t, f.rampStepFixture, refused.ExecutionID)
	assert.Equal(t, config.StatusFailed, got.Status)
	assert.True(t, got.NoSubmission)
	assert.Equal(t, redshiftConflictRefusal, SanitizeStoredError(got.Error), "users see the fixed text verbatim")
	assert.Nil(t, f.stampOf(ctx, t, refused.ExecutionID), "the refusal rolled the stamp back")
}

// Two lineages of one tuple dispatch at once. The first to hold the tuple lock
// waits in the hook until the other is blocked on it, then both gates finish:
// without the lock both would stamp, see no conflict and buy.
func TestRedshiftDispatchGate_TwoLineagesRaceOnlyOneBuys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newRedshiftGateFixture(ctx, t)
	f.raceOnlyOneBuys(ctx, t, f.redshiftRow(ctx, t, "approved"), f.redshiftRow(ctx, t, "approved"))
}

// A root row (key R) and a per-account successor (key R:acct) share one
// family, yet only one may buy. A blanket same-family exclusion would let both.
func TestRedshiftDispatchGate_RootAndAccountSuccessorsOfOneFamilyDoNotBothBuy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newRedshiftGateFixture(ctx, t)
	family := uuid.New().String()
	f.raceOnlyOneBuys(ctx, t,
		f.redshiftRowKeyed(ctx, t, "approved", family, true),
		f.redshiftRowKeyed(ctx, t, "approved", family+":"+f.accounts[0].ID, false))
}

func (f *redshiftGateFixture) raceOnlyOneBuys(ctx context.Context, t *testing.T, rows ...*config.PurchaseExecution) {
	t.Helper()
	var once sync.Once
	afterRedshiftStampHook = func() {
		once.Do(func() {
			assert.EventuallyWithT(t, func(c *assert.CollectT) {
				var waiting int
				require.NoError(c, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
					AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting))
				assert.Positive(c, waiting)
			}, 5*time.Second, 20*time.Millisecond, "the second gate never blocked on the tuple lock")
		})
	}
	// The winner keeps the provider call open until the loser is refused, so
	// the loser's conflict query sees a running row, not a completed one.
	f.beforeBuy = func() {
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			var failed int
			require.NoError(c, f.pool.QueryRow(ctx, `SELECT count(*) FROM purchase_executions WHERE plan_id = $1 AND status = 'failed'`, f.plan.ID).Scan(&failed))
			assert.Positive(c, failed)
		}, 5*time.Second, 20*time.Millisecond, "the loser was never refused")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = f.mgr.claimAndExecute(ctx, rowStatusCopy(row))
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), f.bought.Load(), "exactly one lineage reaches the provider")
	var winners, losers int
	for _, row := range rows {
		got := rowStatus(ctx, t, f.rampStepFixture, row.ExecutionID)
		switch got.Status {
		case "completed":
			winners++
			assert.NotNil(t, f.stampOf(ctx, t, row.ExecutionID))
		case config.StatusFailed:
			losers++
			assert.True(t, got.NoSubmission)
			assert.Nil(t, f.stampOf(ctx, t, row.ExecutionID), "the loser's stamp was rolled back")
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, losers)
}
