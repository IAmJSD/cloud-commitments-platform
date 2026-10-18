//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgresStoreDB_PlanAndSaveSettledTerm checks what build receives from
// PlanAndSaveLadderRun: in-flight plus completed tranches settled after
// ladderSettleAnchor(asOf). Run with LADDER_SETTLED_TEST_DATABASE_URL (see
// TestPostgresStoreDB_LadderSettledAt).
func TestPostgresStoreDB_PlanAndSaveSettledTerm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, _ := settledTestStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	anchor := ladderSettleAnchor(asOf)

	add := func(status ladder.TrancheStatus, amount float64, settled *time.Time) {
		t.Helper()
		tr := lifecycleTranche(cfgID, amount, status, asOf.Add(time.Hour))
		basis := LadderAmountOnDemand
		tr.AmountBasis = &basis
		tr.SettledAt = settled
		require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
	}
	planned := func() float64 {
		t.Helper()
		var got float64
		_, err := store.PlanAndSaveLadderRun(ctx, cfgID, asOf, func(rows []LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
			got = mustOnDemand(t, rows)
			return lockTestRunAt(cfgID, asOf), nil, nil
		})
		require.NoError(t, err)
		return got
	}
	at := func(d time.Duration) *time.Time { v := anchor.Add(d); return &v }

	assert.InDelta(t, 0, planned(), 1e-9, "empty")
	add(ladder.TrancheStatusCompleted, 1, at(-time.Microsecond))
	add(ladder.TrancheStatusCompleted, 2, at(0))
	assert.InDelta(t, 0, planned(), 1e-9, "settled at or before the anchor is already in E")
	add(ladder.TrancheStatusCompleted, 4, at(time.Microsecond))
	assert.InDelta(t, 4, planned(), 1e-9, "just after the anchor counts")
	// Inside the skew margin, before asOf itself: a margin of 0 would miss it.
	add(ladder.TrancheStatusCompleted, 8, at(ladderSettleSkewMargin-time.Second))
	assert.InDelta(t, 12, planned(), 1e-9, "settled within the skew margin before asOf counts")
	add(ladder.TrancheStatusFailed, 16, nil)
	add(ladder.TrancheStatusCancelled, 32, nil)
	assert.InDelta(t, 12, planned(), 1e-9, "failed and canceled never count")
	add(ladder.TrancheStatusFired, 64, nil)
	assert.InDelta(t, 76, planned(), 1e-9, "fired still counts as in-flight, added to the settled term")
}

func lockTestRunAt(cfgID string, at time.Time) *LadderRunDB {
	return &LadderRunDB{ConfigID: &cfgID, StartedAt: at, Status: ladder.RunStatusPlanned}
}
