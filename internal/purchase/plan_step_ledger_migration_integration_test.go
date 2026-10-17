//go:build integration

package purchase

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const neverStartedMigration = "000127_never_started_legacy_ramps_to_new"

// stagedMigrations returns a directory holding every migration that sorts
// before the one under test, and a func that adds it plus every later
// migration, so the test runs the real runner twice. Later migrations must be
// held back too: golang-migrate never applies a version below the current
// one, so staging them in the first run would skip the target.
func stagedMigrations(t *testing.T) (dir string, addTarget func()) {
	t.Helper()
	src := rampMigrationsPath()
	dir = t.TempDir()
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	copyFile := func(name string) {
		b, readErr := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	}
	var held []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		// Zero-padded version prefixes sort lexically in version order.
		if e.Name() < neverStartedMigration {
			copyFile(e.Name())
		} else {
			held = append(held, e.Name())
		}
	}
	return dir, func() {
		for _, name := range held {
			copyFile(name)
		}
	}
}

func TestMigration_NeverStartedLegacyRampsBecomeNew(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Cleanup(ctx) })
	dir, addTarget := stagedMigrations(t)
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), dir, "", ""))
	store, pool := config.NewPostgresStore(container.DB), container.DB.Pool()

	f := newCumFixtureOn(t, store, pool, 100) // plan (a): never started, no executions
	clone := func(name string, total, current int) string {
		p := f.plan()
		p.ID, p.Name = "", name
		p.RampSchedule.TotalSteps = total
		require.NoError(t, store.CreatePurchasePlan(ctx, p))
		_, execErr := pool.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{current_step}',to_jsonb($2::int)) WHERE id=$1`, p.ID, current)
		require.NoError(t, execErr)
		return p.ID
	}
	withExec := clone("b-has-execution", 4, 0)
	require.NoError(t, store.SavePurchaseExecution(ctx, &config.PurchaseExecution{PlanID: withExec, ExecutionID: uuid.NewString(), Status: config.StatusFailed, NoSubmission: true, StepNumber: 1, ScheduledDate: time.Now()}))
	midRamp := clone("c-mid-ramp", 4, 2)
	singleStep := clone("d-single-step", 1, 0)
	trusted := clone("e-trusted", 4, 0)
	withBaseline := clone("f-legacy-baseline", 4, 0)

	// Force the states the old writer left behind; the guard would otherwise rewrite them.
	_, err = pool.Exec(ctx, `ALTER TABLE purchase_plans DISABLE TRIGGER guard_ramp_ledger_state`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE purchase_plans SET ramp_ledger_state='legacy' WHERE id <> $1`, trusted)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE purchase_plans SET ramp_baseline='{"k":{"count":1}}'::jsonb WHERE id = $1`, withBaseline)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE purchase_plans SET ramp_ledger_state='trusted', ramp_baseline='{"k":{"count":1}}'::jsonb WHERE id = $1`, trusted)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE purchase_plans ENABLE TRIGGER guard_ramp_ledger_state`)
	require.NoError(t, err)

	addTarget()
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), dir, "", ""))

	state := func(id string) (s string) {
		require.NoError(t, pool.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, id).Scan(&s))
		return s
	}
	require.Equal(t, "new", state(f.planID), "never started, no executions")
	require.Equal(t, "legacy", state(withExec), "an execution row of any status keeps it legacy")
	require.Equal(t, "legacy", state(midRamp), "mid-ramp plans are not touched")
	require.Equal(t, "legacy", state(singleStep), "single-step plans are not protected")
	require.Equal(t, "legacy", state(withBaseline), "a frozen baseline is never discarded")
	require.Equal(t, "trusted", state(trusted))
	var enabled string
	require.NoError(t, pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger WHERE tgname='guard_ramp_ledger_state'`).Scan(&enabled))
	require.Equal(t, "O", enabled, "the guard is live again")

	rec := cumRec(f.account, "m5.t0", 100)
	f.src.shown = func() []config.RecommendationRecord { return []config.RecommendationRecord{rec} }
	exec, err := f.tick()
	require.NoError(t, err, "the promoted ramp sizes step 1 normally")
	require.NotEmpty(t, exec.Recommendations)
	base, err := f.baseline()
	require.NoError(t, err)
	require.NotEmpty(t, base, "step 1 froze a baseline")
}
