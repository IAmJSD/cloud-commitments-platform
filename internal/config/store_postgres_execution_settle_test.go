package config

import (
	"testing"

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
