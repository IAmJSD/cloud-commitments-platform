//go:build integration

package config

// Real PostgreSQL test for GetLadderExecutionGate (platform#118 PR4r). The
// lookup must compare the idempotency key as TEXT: a UUID-typed compare raises
// 22P02 on every non-UUID key and would make the executor refuse every
// non-ladder purchase.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetLadderExecutionGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "731111111111")
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &cfgID, StartedAt: time.Now().UTC(), Status: ladder.RunStatusExecuting})
	require.NoError(t, err)
	tr := lifecycleTranche(cfgID, 1, ladder.TrancheStatusFired, time.Now().UTC())
	tr.RunID = &run.ID
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))

	t.Run("ladder key returns the config switches and run status", func(t *testing.T) {
		g, err := store.GetLadderExecutionGate(ctx, tr.ID)
		require.NoError(t, err)
		require.NotNil(t, g)
		require.NotNil(t, g.ConfigEnabled)
		require.NotNil(t, g.ConfigMode)
		require.NotNil(t, g.RunStatus)
		assert.True(t, *g.ConfigEnabled)
		assert.Equal(t, "email_approval", *g.ConfigMode)
		assert.Equal(t, "executing", *g.RunStatus)
		require.NotNil(t, g.TrancheStatus)
		assert.Equal(t, "fired", *g.TrancheStatus)
	})

	t.Run("non-ladder keys are not an error", func(t *testing.T) {
		for _, key := range []string{"", tr.ID + ":123456789012", "legacy-exec-key", "not-a-uuid", uuid.NewString()} {
			g, err := store.GetLadderExecutionGate(ctx, key)
			require.NoError(t, err, "key %q must compare as text, not as a UUID", key)
			assert.Nil(t, g, "key %q is not a ladder tranche", key)
		}
	})

	t.Run("canceled run is reported", func(t *testing.T) {
		_, err := store.TransitionLadderRunStatus(ctx, run.ID, []ladder.RunStatus{ladder.RunStatusExecuting}, ladder.RunStatusCancelled)
		require.NoError(t, err)
		g, err := store.GetLadderExecutionGate(ctx, tr.ID)
		require.NoError(t, err)
		require.NotNil(t, g.RunStatus)
		assert.Equal(t, string(ladder.RunStatusCancelled), *g.RunStatus)
	})

	t.Run("deleted config and run come back nil, not false", func(t *testing.T) {
		_, err := store.db.Exec(ctx, `DELETE FROM ladder_runs WHERE id = $1`, run.ID)
		require.NoError(t, err)
		_, err = store.db.Exec(ctx, `DELETE FROM ladder_configs WHERE id = $1`, cfgID)
		require.NoError(t, err)
		g, err := store.GetLadderExecutionGate(ctx, tr.ID)
		require.NoError(t, err)
		require.NotNil(t, g, "the tranche still exists")
		assert.Nil(t, g.ConfigEnabled)
		assert.Nil(t, g.ConfigMode)
		assert.Nil(t, g.RunStatus)
	})
}
