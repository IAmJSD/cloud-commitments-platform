//go:build integration

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingApprovalNotifier holds SendPurchaseApprovalRequest until the test
// releases it, then fails the send, so a test can land another writer on the
// row while the approval email is in flight (issue #1232).
type blockingApprovalNotifier struct {
	stubEmailNotifier
	entered, release chan struct{}
}

func (n *blockingApprovalNotifier) SendPurchaseApprovalRequest(ctx context.Context, _ email.NotificationData) error {
	close(n.entered)
	select {
	case <-n.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return errors.New("ses throttled")
}

// claimRunningLikeApprove is the claim CAS a session approve runs before it
// calls the provider (purchase.Manager.claimRunning): pending|notified ->
// running. Winning it means the commitment is being bought.
func claimRunningLikeApprove(ctx context.Context, store *config.PostgresStore, id string) error {
	_, err := store.TransitionExecutionStatus(ctx, id, []string{"pending", "notified"}, "running", nil)
	return err
}

func storedStatus(t *testing.T, store *config.PostgresStore, id string) *config.PurchaseExecution {
	t.Helper()
	row, err := store.GetExecutionByID(context.Background(), id)
	require.NoError(t, err)
	return row
}

// TestFinalizePurchaseStatus_ApproveDuringEmailSend drives the real send path:
// the submit inserts a pending row, the approval email blocks, an approve
// claims the row (running) while it is in flight, then the send fails. Before
// the fix finalizePurchaseStatus stamped "failed" over the running row, which
// made a commitment that was being bought eligible for retry.
func TestFinalizePurchaseStatus_ApproveDuringEmailSend(t *testing.T) {
	_, store := setupSubmitDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	notify := "approvals@example.com"
	cfg.NotificationEmail = &notify
	require.NoError(t, store.SaveGlobalConfig(ctx, cfg))

	recs, key := submitFixture(t)
	exec := newSubmitExecution(t, recs)
	notifier := &blockingApprovalNotifier{entered: make(chan struct{}), release: make(chan struct{})}
	h := &Handler{config: store, emailNotifier: notifier}
	dup, err := h.persistExecutionAndSuppressions(ctx, exec, nil, "", key)
	require.NoError(t, err)
	require.Nil(t, dup)

	type outcome struct {
		emailSent bool
		status    string
	}
	done := make(chan outcome, 1)
	go func() {
		sent, reason, _ := h.sendPurchaseApprovalEmail(ctx, &events.LambdaFunctionURLRequest{}, exec, "raw-token", recs, 0, 0)
		done <- outcome{sent, h.finalizePurchaseStatus(ctx, exec, sent, reason)}
	}()
	select {
	case <-notifier.entered:
	case <-ctx.Done():
		t.Fatal("approval email send never started")
	}
	require.NoError(t, claimRunningLikeApprove(ctx, store, exec.ExecutionID), "approve must win a pending row")
	close(notifier.release)

	var got outcome
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("send + finalize did not finish")
	}
	assert.False(t, got.emailSent)
	assert.Equal(t, "running", got.status, "the response must report the row's real status")
	row := storedStatus(t, store, exec.ExecutionID)
	assert.Equal(t, "running", row.Status, "a failed email must not overwrite an approved, running row")
	assert.Empty(t, row.Error)
}

// TestFinalizePurchaseStatus_ConcurrentWithApproveClaim races the guarded
// failed write against the approve claim on real Postgres. Exactly one may
// win: either the row is failed and the approve was refused, or the approve
// claimed it and the row stays running. Before the fix the unguarded write
// could land after a won claim, leaving a failed row for a live purchase.
func TestFinalizePurchaseStatus_ConcurrentWithApproveClaim(t *testing.T) {
	_, store := setupSubmitDB(t)
	ctx := context.Background()
	h := &Handler{config: store}
	const rounds = 40
	claimWins := 0
	for i := 0; i < rounds; i++ {
		recs, _ := submitFixture(t)
		exec := newSubmitExecution(t, recs)
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var claimErr error
		var status string
		wg.Add(2)
		go func() { defer wg.Done(); <-start; claimErr = claimRunningLikeApprove(ctx, store, exec.ExecutionID) }()
		go func() {
			defer wg.Done()
			<-start
			status = h.finalizePurchaseStatus(ctx, exec, false, "send failed: ses throttled")
		}()
		close(start)
		wg.Wait()

		row := storedStatus(t, store, exec.ExecutionID)
		if claimErr == nil {
			claimWins++
			assert.Equal(t, "running", row.Status, "round %d: a won approve claim must not be overwritten with failed", i)
			assert.Equal(t, "running", status, "round %d", i)
			continue
		}
		assert.ErrorIs(t, claimErr, config.ErrExecutionNotInExpectedStatus, "round %d", i)
		assert.Equal(t, "failed", row.Status, "round %d", i)
		assert.Equal(t, "failed", status, "round %d", i)
	}
	t.Logf("approve claim won %d of %d rounds", claimWins, rounds)
}
