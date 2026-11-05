package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBlocksManagedDelete(t *testing.T) {
	cases := []struct {
		status       string
		noSubmission bool
		settled      bool
		blocks       bool
	}{
		{StatusFailed, false, false, true},
		{StatusFailed, false, true, false},
		{StatusFailed, true, false, false},
		{"pending", false, true, true},
		{"running", false, true, true},
		{StatusCompleted, false, false, false},
		{StatusPartiallyCompleted, false, false, false},
		{StatusCanceled, false, false, false},
		{StatusExpired, false, false, false},
		{"some_future_status", false, true, true},
	}
	for _, tc := range cases {
		e := &PurchaseExecution{Status: tc.status, NoSubmission: tc.noSubmission}
		require.Equal(t, tc.blocks, blocksManagedDelete(e, tc.settled), "%+v", tc)
	}
}

func TestSettlementRecordReplayMatchesOnlyTheSameReason(t *testing.T) {
	req := ExecutionSettleRequest{Reason: `checked "eu" account`}
	record := settlementRecord(req, settleChainTail{id: "tail", status: StatusCompleted})
	require.Equal(t, `operator:settled_unknown_outcome: ack=true chain_tail=tail tail_status=completed reason="checked \"eu\" account"`, record)
	require.True(t, settlementHasReason(record, req.Reason))
	require.False(t, settlementHasReason(record, "account"))
	require.False(t, settlementHasReason(record, `checked "eu" account2`))
}

// The conflict text is the 409 body, and the caller may not be the earlier
// settler, so it must not carry that settler's principal ID.
func TestReplaySettlementConflictOmitsTheEarlierSettler(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	by := "settler-principal-7f3a"
	record := settlementRecord(ExecutionSettleRequest{Reason: "checked the account"}, settleChainTail{id: "e1", status: "completed"})
	target := &settleTarget{status: "failed", settledAt: &at, settledBy: &by, settlement: &record}
	for name, req := range map[string]ExecutionSettleRequest{
		"another actor":  {ExecutionID: "e1", Actor: "someone-else", Reason: "checked the account"},
		"another reason": {ExecutionID: "e1", Actor: by, Reason: "a different reason"},
	} {
		_, err := replaySettlement(req, target)
		require.ErrorIs(t, err, ErrExecutionSettleConflict, name)
		require.False(t, strings.Contains(err.Error(), by), "%s: %q names the earlier settler", name, err.Error())
		require.Contains(t, err.Error(), "2026-11-04T12:00:00Z", name)
	}
}
