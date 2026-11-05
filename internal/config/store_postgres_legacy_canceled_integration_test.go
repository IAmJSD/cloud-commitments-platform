//go:build integration

package config

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// legacyActorColumn is the pre-migration-000089 column, split so the
// US-locale misspell linter does not flag it.
const legacyActorColumn = "cancel" + "led_by"

// versionBeforeCanceledContract is the schema a deployment is at when the
// build under test starts: the highest migration below the canceled contract
// (the file ending _contract_canceled_spelling.up.sql), which narrows the
// status CHECKs. The cutoff is read from that file, so a renumber needs no edit
// here. The new build must read and write correctly there, because the
// migration and the release roll out in either order.
func versionBeforeCanceledContract(t *testing.T) uint {
	t.Helper()
	return versionBelowCanceledContract(t, getTestMigrationsPath())
}

func versionBelowCanceledContract(t *testing.T, dir string) uint {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	prefix := func(name string) (uint, bool) {
		v, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		return uint(v), err == nil
	}
	var contract uint
	for _, e := range entries {
		if v, ok := prefix(e.Name()); ok && strings.HasSuffix(e.Name(), "_contract_canceled_spelling.up.sql") {
			contract = v
		}
	}
	require.NotZero(t, contract, "no *_contract_canceled_spelling.up.sql migration in %s", dir)
	var best uint
	for _, e := range entries {
		if v, ok := prefix(e.Name()); ok && v < contract && v > best {
			best = v
		}
	}
	require.NotZero(t, best, "no migration below %d", contract)
	return best
}

// TestPostgresStore_NewCodeAgainstPreContractSchema runs the new store code on
// the schema before 000157 with rows an older build wrote: the legacy status
// spelling and an actor only in the legacy column. Every reader must treat them as
// canceled with their actor, and the new writers must not lose them.
func TestPostgresStore_NewCodeAgainstPreContractSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getTestMigrationsPath(), versionBeforeCanceledContract(t)))
	store := NewPostgresStore(container.DB)

	acct := &CloudAccount{Name: "Legacy Cancel", Provider: "aws", ExternalID: "ext-legacy-cancel", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))
	plan := &PurchasePlan{
		Name: "Legacy Cancel Plan", Services: map[string]ServiceConfig{},
		RampSchedule: RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, CurrentStep: 2, TotalSteps: 4},
	}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))

	legacyRow := func(updatedAt time.Time, step int, actor string) string {
		t.Helper()
		var id string
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO purchase_executions (plan_id, status, step_number, scheduled_date, cloud_account_id, `+legacyActorColumn+`, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING execution_id::text`,
			plan.ID, LegacyStatusCanceled, step, updatedAt, acct.ID, actor, updatedAt).Scan(&id))
		return id
	}
	rawActors := func(id string) (canceledBy, legacyBy *string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT canceled_by, `+legacyActorColumn+` FROM purchase_executions WHERE execution_id = $1`, id).Scan(&canceledBy, &legacyBy))
		return canceledBy, legacyBy
	}
	now := time.Now().UTC().Truncate(time.Second)
	legacy := legacyRow(now, 3, "legacy@example.com")

	t.Run("get reads the actor through COALESCE", func(t *testing.T) {
		got, err := store.GetExecutionByID(ctx, legacy)
		require.NoError(t, err)
		assert.Equal(t, LegacyStatusCanceled, got.Status)
		require.NotNil(t, got.CancelledBy, "a legacy-column-only actor must scan back")
		assert.Equal(t, "legacy@example.com", *got.CancelledBy)
	})

	t.Run("history list and summary include the legacy status", func(t *testing.T) {
		statuses := []string{StatusCanceled, LegacyStatusCanceled}
		page, err := store.ListHistoryExecutionsPage(ctx, HistoryExecutionsPageQuery{Statuses: statuses, Limit: 10})
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.NotNil(t, page[0].CancelledBy)
		assert.Equal(t, "legacy@example.com", *page[0].CancelledBy)

		groups, err := store.HistorySummaryAggregate(ctx, HistorySummaryFilter{ExecutionStatuses: statuses})
		require.NoError(t, err)
		require.Len(t, groups.Executions, 1)
		assert.Equal(t, LegacyStatusCanceled, groups.Executions[0].Status)
		assert.Equal(t, 1, groups.Executions[0].Count)
	})

	t.Run("a legacy row is terminal and holds no capacity or ramp step", func(t *testing.T) {
		live, err := store.CountLiveExecutionsForAccount(ctx, acct.ID)
		require.NoError(t, err)
		assert.Zero(t, live, "a legacy canceled row must not block deleting the account")

		assert.False(t, holdsCapacity(&PurchaseExecution{Status: LegacyStatusCanceled}))

		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		occupied, err := store.OccupiedRampStepsInRangeTx(ctx, tx, plan.ID, 3, 3)
		require.NoError(t, err)
		assert.Empty(t, occupied, "a step whose only unit is legacy canceled settled without buying")
	})

	t.Run("save keeps a legacy actor readable and never erases an actor", func(t *testing.T) {
		got, err := store.GetExecutionByID(ctx, legacy)
		require.NoError(t, err)
		got.Status = StatusCanceled
		require.NoError(t, store.SavePurchaseExecution(ctx, got))
		canceledBy, legacyBy := rawActors(legacy)
		require.NotNil(t, canceledBy, "the save carries the legacy actor into canceled_by")
		assert.Equal(t, "legacy@example.com", *canceledBy)
		require.NotNil(t, legacyBy, "the legacy column is left as the older build wrote it")
		assert.Equal(t, "legacy@example.com", *legacyBy)
		reread, err := store.GetExecutionByID(ctx, legacy)
		require.NoError(t, err)
		require.NotNil(t, reread.CancelledBy)
		assert.Equal(t, "legacy@example.com", *reread.CancelledBy)

		// A save from a copy read before a concurrent cancel stamped the actor.
		stale := *got
		stale.CancelledBy = nil
		_, err = pool.Exec(ctx, `UPDATE purchase_executions SET canceled_by = 'concurrent@example.com' WHERE execution_id = $1`, legacy)
		require.NoError(t, err)
		require.NoError(t, store.SavePurchaseExecution(ctx, &stale))
		canceledBy, _ = rawActors(legacy)
		require.NotNil(t, canceledBy)
		assert.Equal(t, "concurrent@example.com", *canceledBy, "a stale save must not erase the actor")

		// A stale copy that carries a DIFFERENT actor must not replace the
		// stored one either: the stored actor wins over the saved $17.
		other := *got
		otherActor := "stale-copy@example.com"
		other.CancelledBy = &otherActor
		require.NoError(t, store.SavePurchaseExecution(ctx, &other))
		canceledBy, _ = rawActors(legacy)
		require.NotNil(t, canceledBy)
		assert.Equal(t, "concurrent@example.com", *canceledBy, "a stale save with another actor must not replace the stored actor")
	})

	t.Run("cleanup removes a legacy row after retention", func(t *testing.T) {
		old := legacyRow(now.AddDate(0, 0, -31), 1, "old@example.com")
		deleted, err := store.CleanupOldExecutions(ctx, 30)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted)
		_, err = store.GetExecutionByID(ctx, old)
		assert.ErrorIs(t, err, ErrNotFound)
		_, err = store.GetExecutionByID(ctx, legacy)
		assert.NoError(t, err, "a fresh legacy row survives")
	})
}
