package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The chain-node allowlist fails closed: a live status or one added later is
// not resolvable (the status CHECK keeps such rows out of the database tests).
func TestLadderResolvableNodeStatus(t *testing.T) {
	for _, s := range []string{StatusFailed, StatusCanceled, StatusExpired, StatusCompleted, StatusPartiallyCompleted} {
		assert.True(t, ladderResolvableNodeStatus(s), s)
	}
	for _, s := range []string{"pending", "notified", "scheduled", "approved", "running", "", "future-status"} {
		assert.False(t, ladderResolvableNodeStatus(s), s)
	}
}
