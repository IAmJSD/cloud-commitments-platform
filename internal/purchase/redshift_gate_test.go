package purchase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func gateRec(account string, count int) config.RecommendationRecord {
	r := redshiftScopedRec(account)
	r.Region, r.Term, r.Payment, r.Count = "us-east-1", 1, "no-upfront", count
	return r
}

type gateRun struct {
	err    error
	exec   *config.PurchaseExecution
	finds  []config.RedshiftTuple
	family string
	locked []config.RedshiftTuple
}

// runGate drives the gate with a stamp result and the conflicts the query returns.
func runGate(t *testing.T, exec *config.PurchaseExecution, stamp config.StampResult, conflicts []config.RedshiftLineageConflict) *gateRun {
	t.Helper()
	run := &gateRun{exec: exec}
	store := new(MockConfigStore)
	store.On("LockRedshiftTuplesTx", mock.Anything, mock.Anything, mock.Anything).
		Run(func(a mock.Arguments) { run.locked = a.Get(2).([]config.RedshiftTuple) }).Return(nil)
	store.On("StampFirstDispatchTx", mock.Anything, mock.Anything, exec.ExecutionID, redshiftRedriveHorizon).Return(stamp, nil)
	store.On("FindOpenRedshiftLineageConflictsTx", mock.Anything, mock.Anything, exec.ExecutionID, mock.Anything,
		mock.Anything, mock.Anything, redshiftRedriveHorizon).
		Run(func(a mock.Arguments) { run.family = a.String(3); run.finds = a.Get(4).([]config.RedshiftTuple) }).
		Return(conflicts, nil).Maybe()
	run.err = (&Manager{config: store}).stampRedshiftDispatch(context.Background(), exec)
	return run
}

func stamped() config.StampResult {
	at := time.Now().UTC().Add(-time.Hour)
	return config.StampResult{At: &at, WithinHorizon: true}
}

func gateExec(attempt int, recs ...config.RecommendationRecord) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID: "rs-self", IdempotencyKey: "fam-1:acct-A", RetryAttemptN: attempt, Recommendations: recs,
	}
}

func isBeforeSubmission(err error) bool {
	var bs *beforeSubmissionError
	return errors.As(err, &bs)
}

func TestRedshiftDispatchGate_ConflictRefusesBeforeSubmission(t *testing.T) {
	for _, attempt := range []int{0, 1} {
		exec := gateExec(attempt, gateRec("acct-A", 2))
		run := runGate(t, exec, stamped(), []config.RedshiftLineageConflict{{ExecutionID: "other", Status: "running"}})

		require.Error(t, run.err)
		assert.True(t, isBeforeSubmission(run.err), "attempt %d", attempt)
		assert.Equal(t, redshiftConflictRefusal, run.err.Error(), "the text names no other execution")
		assert.Nil(t, exec.FirstDispatchAt, "a refused dispatch records no stamp")
		assert.Equal(t, "fam-1", run.family)
	}
}

func TestRedshiftDispatchGate_QueriesOnlySelectedRedshiftTuples(t *testing.T) {
	unselected := gateRec("acct-A", 7)
	unselected.Selected = false
	ec2 := scopedTestRec("acct-A")
	exec := gateExec(0, gateRec("acct-A", 2), gateRec("acct-A", 3), unselected, ec2)
	run := runGate(t, exec, stamped(), nil)

	require.NoError(t, run.err)
	require.NotNil(t, exec.FirstDispatchAt)
	require.Len(t, run.finds, 2)
	assert.Equal(t, 2, run.finds[0].Count)
	assert.Equal(t, 3, run.finds[1].Count)
}

func TestRedshiftDispatchGate_SameFamilySiblingFailedRowAllowed(t *testing.T) {
	exec := gateExec(1, gateRec("acct-A", 2))
	run := runGate(t, exec, stamped(), nil)

	require.NoError(t, run.err)
	assert.Equal(t, "fam-1", run.family, "the family root lets the query exclude same-family non-running rows")
}

func TestRedshiftDispatchGate_RetrySuccessorWithoutStampRefusedNotBeforeSubmission(t *testing.T) {
	exec := gateExec(1, gateRec("acct-A", 2))
	run := runGate(t, exec, config.StampResult{}, nil)

	require.Error(t, run.err)
	assert.False(t, isBeforeSubmission(run.err), "no_submission must stay false")
	assert.Nil(t, exec.FirstDispatchAt)
}

func TestRedshiftDispatchGate_PastHorizonRefusedAtEveryAttempt(t *testing.T) {
	for _, attempt := range []int{0, 1} {
		at := time.Now().UTC().Add(-30 * time.Hour)
		exec := gateExec(attempt, gateRec("acct-A", 2))
		run := runGate(t, exec, config.StampResult{At: &at, WithinHorizon: false}, nil)

		require.Error(t, run.err)
		assert.False(t, isBeforeSubmission(run.err), "attempt %d: an old stamp proves an earlier dispatch", attempt)
		assert.Equal(t, redshiftHorizonRefusal, run.err.Error())
		assert.Nil(t, exec.FirstDispatchAt)
	}
}

func TestRedshiftDispatchGate_LocksEveryTupleAndPassesRecAccounts(t *testing.T) {
	recB := gateRec("acct-B", 2)
	exec := gateExec(0, gateRec("acct-A", 2), recB)
	run := runGate(t, exec, stamped(), nil)

	require.NoError(t, run.err)
	require.Len(t, run.locked, 2)
	require.Len(t, run.finds, 2)
	require.NotNil(t, run.finds[1].CloudAccountID)
	assert.Equal(t, *recB.CloudAccountID, *run.finds[1].CloudAccountID)
}

func TestRedshiftDispatchGate_StoreErrorsByAttempt(t *testing.T) {
	for _, failing := range []string{"LockRedshiftTuplesTx", "FindOpenRedshiftLineageConflictsTx"} {
		for _, attempt := range []int{0, 1} {
			store := new(MockConfigStore)
			boom := errors.New("db down")
			lockErr, findErr := error(nil), error(nil)
			if failing == "LockRedshiftTuplesTx" {
				lockErr = boom
			} else {
				findErr = boom
			}
			store.On("LockRedshiftTuplesTx", mock.Anything, mock.Anything, mock.Anything).Return(lockErr)
			store.On("StampFirstDispatchTx", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(stamped(), nil).Maybe()
			store.On("FindOpenRedshiftLineageConflictsTx", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything, mock.Anything, mock.Anything).Return(nil, findErr).Maybe()
			exec := gateExec(attempt, gateRec("acct-A", 2))

			err := (&Manager{config: store}).stampRedshiftDispatch(context.Background(), exec)

			require.ErrorIs(t, err, boom)
			assert.Equal(t, attempt == 0, isBeforeSubmission(err), "%s attempt %d", failing, attempt)
			assert.Nil(t, exec.FirstDispatchAt)
		}
	}
}

func TestRedshiftDispatchGate_CallOrderIsLockStampFind(t *testing.T) {
	store := new(MockConfigStore)
	var order []string
	note := func(name string) func(mock.Arguments) { return func(mock.Arguments) { order = append(order, name) } }
	store.On("LockRedshiftTuplesTx", mock.Anything, mock.Anything, mock.Anything).Run(note("lock")).Return(nil)
	store.On("StampFirstDispatchTx", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(note("stamp")).Return(stamped(), nil)
	store.On("FindOpenRedshiftLineageConflictsTx", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything).Run(note("find")).Return(nil, nil)

	require.NoError(t, (&Manager{config: store}).stampRedshiftDispatch(context.Background(), gateExec(0, gateRec("acct-A", 2))))
	assert.Equal(t, []string{"lock", "stamp", "find"}, order)
}

func TestRedshiftTuple_LockKeyDiffersPerField(t *testing.T) {
	base := config.RedshiftTuple{Region: "r", NodeType: "n", Term: 1, Payment: "p", Count: 2}
	for name, mut := range map[string]func(*config.RedshiftTuple){
		"region":    func(x *config.RedshiftTuple) { x.Region = "r2" },
		"node type": func(x *config.RedshiftTuple) { x.NodeType = "n2" },
		"term":      func(x *config.RedshiftTuple) { x.Term = 3 },
		"payment":   func(x *config.RedshiftTuple) { x.Payment = "p2" },
		"count":     func(x *config.RedshiftTuple) { x.Count = 3 },
	} {
		other := base
		mut(&other)
		assert.NotEqual(t, base.LockKey(), other.LockKey(), name)
	}
}

func TestRedshiftTuple_LockKeyIgnoresAccount(t *testing.T) {
	a, b := "a", "b"
	t1 := config.RedshiftTuple{Region: "r", NodeType: "n", Term: 1, Payment: "p", Count: 2, CloudAccountID: &a}
	t2 := t1
	t2.CloudAccountID = &b
	assert.Equal(t, t1.LockKey(), t2.LockKey())
	t2.Count = 3
	assert.NotEqual(t, t1.LockKey(), t2.LockKey())
}

func TestRedshiftHorizonRefusal_NamesTheHorizon(t *testing.T) {
	assert.Contains(t, redshiftHorizonRefusal, fmt.Sprintf(" %.0fh ", redshiftRedriveHorizon.Hours()))
}
