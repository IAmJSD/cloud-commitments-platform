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

// TestCancelOrRecoverExecution_LegacyCanceledRowBeforeContractMigration runs
// the cancel-race recovery on the schema before 000159 against a row an older
// build canceled (legacy status spelling, actor only in the legacy column). The
// build and the migration roll out in either order, so the retry must see the
// race as already won, return the canonical status and keep the actor.
func TestCancelOrRecoverExecution_LegacyCanceledRowBeforeContractMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	path := getMigrationsPath()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	var before uint
	for _, e := range entries {
		v, parseErr := strconv.ParseUint(strings.SplitN(e.Name(), "_", 2)[0], 10, 32)
		if parseErr == nil && v < 159 && uint(v) > before {
			before = uint(v)
		}
	}
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
