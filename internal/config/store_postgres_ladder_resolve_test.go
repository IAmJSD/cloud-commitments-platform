package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chain-node allowlist fails closed: a live status or one added later is
// not resolvable (the status CHECK keeps such rows out of the database tests).
func TestLadderResolvableNodeStatus(t *testing.T) {
	for _, s := range []string{StatusFailed, StatusCanceled, LegacyStatusCanceled, StatusExpired, StatusCompleted, StatusPartiallyCompleted} {
		assert.True(t, ladderResolvableNodeStatus(s), s)
	}
	for _, s := range []string{"pending", "notified", "scheduled", "approved", "running", "", "future-status"} {
		assert.False(t, ladderResolvableNodeStatus(s), s)
	}
}

// lastChange comes back from Postgres truncated to microseconds; a check
// stamped with the change's own instant must not count as after it.
func TestValidateLadderUnverifiedFail_CheckedAtTheChangeIsRefused(t *testing.T) {
	change := time.Date(2026, 10, 23, 1, 2, 3, 456789123, time.UTC)
	req := LadderResolveFailed{
		Acknowledge: true,
		Check:       &LadderCloudCheck{AccountID: "acct-1", Region: "us-east-1", Service: "ec2", CheckedAt: change},
		Now:         change.Add(time.Hour),
		StaleAfter:  time.Minute,
	}
	lastChange := change.Truncate(time.Microsecond)

	require.ErrorIs(t, validateLadderUnverifiedFail(req, "ec2", []string{"acct-1"}, lastChange), ErrLadderResolveInvalid)

	req.Check.CheckedAt = change.Add(time.Minute)
	require.NoError(t, validateLadderUnverifiedFail(req, "ec2", []string{"acct-1"}, lastChange))
}
