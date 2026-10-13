package config

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/assert"
)

var allLadderTrancheStatuses = []ladder.TrancheStatus{
	ladder.TrancheStatusScheduled, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted,
	ladder.TrancheStatusCancelled, ladder.TrancheStatusFailed,
}

// Pure unit test (no database): the edge table is the whole state machine.
func TestLadderTrancheEdges_OnlyDocumentedTransitionsAreLegal(t *testing.T) {
	legal := map[[2]ladder.TrancheStatus]bool{
		{ladder.TrancheStatusScheduled, ladder.TrancheStatusFired}:     true,
		{ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled}: true,
		{ladder.TrancheStatusFired, ladder.TrancheStatusCompleted}:     true,
		{ladder.TrancheStatusFired, ladder.TrancheStatusFailed}:        true,
	}
	for _, from := range allLadderTrancheStatuses {
		for _, to := range allLadderTrancheStatuses {
			err := validateLadderTrancheEdge(from, to)
			if legal[[2]ladder.TrancheStatus{from, to}] {
				assert.NoError(t, err, "%s -> %s", from, to)
			} else {
				assert.Error(t, err, "%s -> %s must be illegal", from, to)
			}
		}
	}
}

// Illegal edges and the claim-only edge fail before any SQL runs, so a nil DB
// proves no query was attempted.
func TestTransitionLadderTranche_RejectsIllegalEdgesWithoutQuerying(t *testing.T) {
	s := &PostgresStore{}
	for _, from := range allLadderTrancheStatuses {
		for _, to := range allLadderTrancheStatuses {
			if validateLadderTrancheEdge(from, to) == nil && (from != ladder.TrancheStatusScheduled || to != ladder.TrancheStatusFired) {
				continue
			}
			applied, err := s.TransitionLadderTrancheTx(context.Background(), nil, "id", from, to, "test")
			assert.Error(t, err, "%s -> %s", from, to)
			assert.False(t, applied)
		}
	}
}

func TestLadderGraceCutoff_IsExactComplementOfNettingBound(t *testing.T) {
	asOf := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, asOf.Add(-24*time.Hour), ladderGraceCutoff(asOf))
}

// A failed or canceled transition without a reason is rejected before any SQL.
func TestTransitionLadderTranche_RequiresReasonForFailedAndCanceled(t *testing.T) {
	s := &PostgresStore{}
	for _, c := range []struct{ from, to ladder.TrancheStatus }{
		{ladder.TrancheStatusFired, ladder.TrancheStatusFailed},
		{ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled},
	} {
		applied, err := s.TransitionLadderTrancheTx(context.Background(), nil, "id", c.from, c.to, "")
		assert.ErrorContains(t, err, "needs a status reason", "%s -> %s", c.from, c.to)
		assert.False(t, applied)
	}
}
