//go:build integration
// +build integration

package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL. Issue #1190: the email-link cancel and the immediate
// email-link approve pin the approval token inside their status CAS, as the
// delayed approve already did (issue #590), so a token rotated by a
// re-notification between the caller's snapshot check and the CAS can no
// longer move the row.

const pinOldToken, pinNewToken = "pin-old-raw-token", "pin-new-raw-token"

func tokenPinFixture(t *testing.T) (*PostgresStore, func(t *testing.T, expiresAt time.Time) *PurchaseExecution) {
	t.Helper()
	conn := setupTestContainerDB(t)
	store := NewPostgresStore(conn)
	ctx := context.Background()
	plan := &PurchasePlan{Name: "token pin cancel", Enabled: true, Services: map[string]ServiceConfig{}, RampSchedule: PresetRampSchedules["immediate"]}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	newRow := func(t *testing.T, expiresAt time.Time) *PurchaseExecution {
		t.Helper()
		exec := &PurchaseExecution{PlanID: plan.ID, Status: "notified", StepNumber: 1, ScheduledDate: time.Now(),
			ApprovalToken: HashApprovalToken(pinOldToken), ApprovalTokenExpiresAt: &expiresAt}
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		return exec
	}
	return store, newRow
}

// pinnedCancel runs CancelExecutionAtomicTokenPinned in its own transaction,
// as purchase.Manager.CancelExecution does.
func pinnedCancel(ctx context.Context, store *PostgresStore, id, rawToken string) (canceled bool, status string, err error) {
	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		canceled, status, txErr = store.CancelExecutionAtomicTokenPinned(ctx, tx, id, nil, rawToken)
		return txErr
	})
	return canceled, status, err
}

// requireStillNotified asserts a refused action left the row notified and
// holding rawToken.
func requireStillNotified(ctx context.Context, t *testing.T, store *PostgresStore, id, rawToken string) {
	t.Helper()
	got, err := store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "notified", got.Status)
	assert.True(t, ApprovalTokenMatches(got.ApprovalToken, rawToken), "stored token should still be %q", rawToken)
}

func TestPostgresStoreDB_CancelExecutionAtomicTokenPinned(t *testing.T) {
	store, newRow := tokenPinFixture(t)
	ctx := context.Background()

	t.Run("token rotated after the snapshot loses the cancel", func(t *testing.T) {
		exec := newRow(t, time.Now().Add(time.Hour))
		rotated, err := store.RotatePendingApprovalToken(ctx, exec.ExecutionID, HashApprovalToken(pinNewToken), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.True(t, rotated)

		canceled, _, err := pinnedCancel(ctx, store, exec.ExecutionID, pinOldToken)
		require.ErrorIs(t, err, ErrApprovalTokenChanged)
		assert.False(t, canceled)
		requireStillNotified(ctx, t, store, exec.ExecutionID, pinNewToken)

		canceled, status, err := pinnedCancel(ctx, store, exec.ExecutionID, pinNewToken)
		require.NoError(t, err)
		assert.True(t, canceled, "the current token still cancels")
		assert.Equal(t, StatusCanceled, status)
	})

	t.Run("token expired after the snapshot loses the cancel", func(t *testing.T) {
		exec := newRow(t, time.Now().Add(-time.Minute))
		_, _, err := pinnedCancel(ctx, store, exec.ExecutionID, pinOldToken)
		require.ErrorIs(t, err, ErrApprovalTokenChanged)
		requireStillNotified(ctx, t, store, exec.ExecutionID, pinOldToken)
	})

	t.Run("wrong or empty token loses the cancel", func(t *testing.T) {
		exec := newRow(t, time.Now().Add(time.Hour))
		for _, tok := range []string{"not-the-token", ""} {
			_, _, err := pinnedCancel(ctx, store, exec.ExecutionID, tok)
			require.ErrorIs(t, err, ErrApprovalTokenChanged, "token %q", tok)
			requireStillNotified(ctx, t, store, exec.ExecutionID, pinOldToken)
		}
	})

	t.Run("matching token on a row already approved is a status CAS loss", func(t *testing.T) {
		exec := newRow(t, time.Now().Add(time.Hour))
		_, err := store.TransitionExecutionStatusTokenPinned(ctx, exec.ExecutionID, []string{"pending", "notified"}, "running", nil, pinOldToken)
		require.NoError(t, err)
		canceled, status, err := pinnedCancel(ctx, store, exec.ExecutionID, pinOldToken)
		require.NoError(t, err, "a lost status race is reported through currentStatus, as on the unpinned cancel")
		assert.False(t, canceled)
		assert.Equal(t, "running", status)
	})

	t.Run("the unpinned cancel is unchanged", func(t *testing.T) {
		exec := newRow(t, time.Now().Add(-time.Minute))
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			canceled, _, err := store.CancelExecutionAtomic(ctx, tx, exec.ExecutionID, nil)
			assert.True(t, canceled, "the session cancel is not token-gated")
			return err
		}))
	})
}

// waitForBlockedUpdate waits until some backend is waiting on a row lock, so
// the test knows the second writer has reached the locked row.
func waitForBlockedUpdate(ctx context.Context, t *testing.T, store *PostgresStore) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the second writer never blocked on the row lock")
}

// tokenPinnedActions are the two pinned transitions issue #1190 adds callers
// for: the token cancel and the immediate token approve's claim. Each reports
// whether it moved the row.
var tokenPinnedActions = []struct {
	name   string
	status string
	run    func(ctx context.Context, store *PostgresStore, id, rawToken string) (bool, error)
}{
	{"cancel", StatusCanceled, func(ctx context.Context, store *PostgresStore, id, rawToken string) (bool, error) {
		canceled, _, err := pinnedCancel(ctx, store, id, rawToken)
		return canceled, err
	}},
	{"approve claim", "running", func(ctx context.Context, store *PostgresStore, id, rawToken string) (bool, error) {
		row, err := store.TransitionExecutionStatusTokenPinned(ctx, id, []string{"pending", "notified"}, "running", nil, rawToken)
		return row != nil, err
	}},
}

// The rotation commits while the pinned action is blocked on the row lock:
// PostgreSQL re-evaluates the action's WHERE against the rotated row, so the
// old token must lose even though it matched when the action started.
func TestPostgresStoreDB_TokenPinnedActionBlockedBehindRotationLoses(t *testing.T) {
	store, newRow := tokenPinFixture(t)
	ctx := context.Background()
	for _, action := range tokenPinnedActions {
		t.Run(action.name, func(t *testing.T) {
			exec := newRow(t, time.Now().Add(time.Hour))
			var moved bool
			var actionErr error
			done := make(chan struct{})
			require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, `UPDATE purchase_executions SET approval_token_hash = $2, status = 'notified'
					WHERE execution_id = $1 AND status IN ('pending', 'notified')`, exec.ExecutionID, HashApprovalToken(pinNewToken))
				require.NoError(t, err)
				require.EqualValues(t, 1, tag.RowsAffected())
				go func() {
					defer close(done)
					moved, actionErr = action.run(ctx, store, exec.ExecutionID, pinOldToken)
				}()
				waitForBlockedUpdate(ctx, t, store)
				return nil
			}))
			<-done
			require.ErrorIs(t, actionErr, ErrApprovalTokenChanged)
			assert.False(t, moved)
			requireStillNotified(ctx, t, store, exec.ExecutionID, pinNewToken)
		})
	}
}

// Unsynchronized race: a re-notification rotating the token and an action
// presenting the old token hit the same row at once, many times. Exactly one
// may win. Before issue #1190 the cancel and the immediate approve compared
// the token only against their snapshot, so both could succeed: the row was
// canceled or claimed with a token that had already been revoked.
func TestPostgresStoreDB_TokenPinnedActionRacesRotation(t *testing.T) {
	store, newRow := tokenPinFixture(t)
	ctx := context.Background()
	const rounds = 40
	for _, action := range tokenPinnedActions {
		t.Run(action.name, func(t *testing.T) {
			actionWins, rotationWins := 0, 0
			for i := 0; i < rounds; i++ {
				exec := newRow(t, time.Now().Add(time.Hour))
				var wg sync.WaitGroup
				start := make(chan struct{})
				var moved, rotated bool
				var actionErr, rotateErr error
				// Stagger one side by 0-1.9ms, alternating sides, so both
				// orderings (and the overlap between them) are exercised.
				lag := time.Duration(i%20) * 100 * time.Microsecond
				actionLag, rotateLag := lag, time.Duration(0)
				if i%2 == 1 {
					actionLag, rotateLag = 0, lag
				}
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					time.Sleep(actionLag)
					moved, actionErr = action.run(ctx, store, exec.ExecutionID, pinOldToken)
				}()
				go func() {
					defer wg.Done()
					<-start
					time.Sleep(rotateLag)
					rotated, rotateErr = store.RotatePendingApprovalToken(ctx, exec.ExecutionID, HashApprovalToken(pinNewToken), time.Now().Add(time.Hour))
				}()
				close(start)
				wg.Wait()
				require.NoError(t, rotateErr)
				if actionErr != nil {
					require.ErrorIs(t, actionErr, ErrApprovalTokenChanged, "round %d", i)
				}
				require.True(t, moved != rotated, "round %d: exactly one of the action (%v) and the rotation (%v) may win", i, moved, rotated)
				got, err := store.GetExecutionByID(ctx, exec.ExecutionID)
				require.NoError(t, err)
				if moved {
					actionWins++
					assert.Equal(t, action.status, got.Status)
					assert.True(t, ApprovalTokenMatches(got.ApprovalToken, pinOldToken), "a refused rotation must not have written its token")
				} else {
					rotationWins++
					assert.Equal(t, "notified", got.Status, "the rotated row must be left for the new token")
					assert.True(t, ApprovalTokenMatches(got.ApprovalToken, pinNewToken))
				}
			}
			t.Logf("%s: action won %d, rotation won %d of %d rounds", action.name, actionWins, rotationWins, rounds)
			assert.Positive(t, actionWins, "the race never let the action go first; the rounds proved nothing about that ordering")
			assert.Positive(t, rotationWins, "the race never let the rotation go first; the rounds proved nothing about that ordering")
		})
	}
}
