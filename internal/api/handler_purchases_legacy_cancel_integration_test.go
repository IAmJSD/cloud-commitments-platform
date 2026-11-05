//go:build integration

package api

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// versionBeforeCanceledContract is the schema a deployment is at when the
// build under test starts: the highest migration below the canceled contract
// (the file ending _contract_canceled_spelling.up.sql), which narrows the
// status CHECKs. The cutoff is read from that file, so a renumber needs no edit
// here. The new build must read and write correctly there, because the
// migration and the release roll out in either order.
func versionBeforeCanceledContract(t *testing.T, dir string) uint {
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

// TestCancelOrRecoverExecution_LegacyCanceledRowBeforeContractMigration runs
// the cancel-race recovery on the schema before 000155 against a row an older
// build canceled (legacy status spelling, actor only in the legacy column). The
// build and the migration roll out in either order, so the retry must see the
// race as already won, return the canonical status and keep the actor.
func TestCancelOrRecoverExecution_LegacyCanceledRowBeforeContractMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	path := getMigrationsPath()
	before := versionBeforeCanceledContract(t, path)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, before))
	store := config.NewPostgresStore(container.DB)

	var execID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date, `+"cancel"+`led_by)
		VALUES ($1, NOW(), 'legacy@example.com') RETURNING execution_id::text`,
		config.LegacyStatusCanceled).Scan(&execID))

	h := &Handler{config: store}
	got, err := h.cancelOrRecoverExecution(ctx, execID, nil)
	require.NoError(t, err, "a legacy canceled row is an already-won cancel race, not a 409")
	require.Equal(t, config.StatusCanceled, got.Status, "the idempotent path returns the canonical spelling")
	require.NotNil(t, got.CancelledBy)
	require.Equal(t, "legacy@example.com", *got.CancelledBy)
}
