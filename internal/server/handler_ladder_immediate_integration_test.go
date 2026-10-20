//go:build integration

package server

// Real PostgreSQL tests for ladder PR B1 (immediate ramp steps persisted as
// tranches). executeLadderRun, the concrete PostgresStore and the migrated
// schema are real; the ladder capability is a fixture (labeled FIXTURE).
// No purchase path exists: the executor is not built and nothing fires.
//
// Database: set LADDER_B1_TEST_DATABASE_URL to a THROWAWAY local database
// whose name starts with "ladder_b1_"; otherwise a Postgres container is used.

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ladderB1Target = 10.0 // FIXTURE: low-water 12.5 * 80% coverage, existing 0

func ladderB1Conn(ctx context.Context, t *testing.T) *database.Connection {
	t.Helper()
	dsn := os.Getenv("LADDER_B1_TEST_DATABASE_URL")
	if dsn == "" {
		c := testhelpers.RequirePostgresContainer(ctx, t)
		t.Cleanup(func() { _ = c.Cleanup(context.Background()) })
		require.NoError(t, migrations.RunMigrations(ctx, c.DB.Pool(), "../database/postgres/migrations", "", ""))
		return c.DB
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "ladder_b1_"), "refusing a non-throwaway database")
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	pw, _ := u.User.Password()
	conn, err := database.NewConnection(ctx, &database.Config{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: pw, SSLMode: "disable", MaxConnections: 5,
		HealthCheckPeriod: time.Minute, MaxConnLifetime: time.Hour, MaxConnIdleTime: 30 * time.Minute,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	require.NoError(t, migrations.RunMigrations(ctx, conn.Pool(), "../database/postgres/migrations", "", ""))
	return conn
}

type ladderB1Env struct {
	conn  *database.Connection
	app   *Application
	cfg   *config.LadderConfigDB
	cap   *fakeLadderCapability
	cap2  pkgladder.LadderCapability // when set, run uses it instead of cap
	start time.Time
}

// newLadderB1Env seeds a fresh account and config with the given ramp. The
// FIXTURE capability yields a single ConvertibleRI allocation of gap 10.
func newLadderB1Env(ctx context.Context, t *testing.T, conn *database.Connection, ramp []pkgladder.RampStep) *ladderB1Env {
	t.Helper()
	acct := uuid.New().String()
	_, err := conn.Exec(ctx, `INSERT INTO cloud_accounts (id, name, enabled, provider, external_id, aws_is_org_root, created_at, updated_at)
		VALUES ($1, 'b1', true, 'aws', $2, false, now(), now())`, acct, strconv.FormatInt(time.Now().UnixNano()%1e12, 10))
	require.NoError(t, err)
	rampJSON, err := json.Marshal(pkgladder.RampSchedule{Steps: ramp})
	require.NoError(t, err)
	store := config.NewPostgresStore(conn)
	cfg := validTestDBConfig("")
	cfg.CloudAccountID = acct
	cfg.RampSchedule = rampJSON
	saved, err := store.UpsertLadderConfig(ctx, &cfg)
	require.NoError(t, err)
	return &ladderB1Env{
		conn: conn, app: &Application{Config: store}, cfg: saved,
		cap:   &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5)},
		start: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func (e *ladderB1Env) run(ctx context.Context, t *testing.T, now time.Time) string {
	t.Helper()
	var capability pkgladder.LadderCapability = e.cap
	if e.cap2 != nil {
		capability = e.cap2
	}
	require.NoError(t, e.app.executeLadderRun(ctx, e.cfg, capability, "123456789012", pkgladder.Term1Year, pkgladder.PaymentNoUpfront, now))
	var id string
	require.NoError(t, e.conn.QueryRow(ctx, `SELECT id FROM ladder_runs WHERE config_id=$1 ORDER BY started_at DESC, created_at DESC LIMIT 1`, e.cfg.ID).Scan(&id))
	return id
}

type b1Row struct {
	amount float64
	at     time.Time
	status string
}

func (e *ladderB1Env) rows(ctx context.Context, t *testing.T, runID string) []b1Row {
	t.Helper()
	rs, err := e.conn.Query(ctx, `SELECT amount_usd_hr::float8, scheduled_date, status FROM ladder_tranches WHERE run_id=$1 ORDER BY scheduled_date`, runID)
	require.NoError(t, err)
	defer rs.Close()
	var out []b1Row
	for rs.Next() {
		var r b1Row
		require.NoError(t, rs.Scan(&r.amount, &r.at, &r.status))
		out = append(out, r)
	}
	require.NoError(t, rs.Err())
	return out
}

// live is the sum of rows that are not canceled: what could still be bought.
func (e *ladderB1Env) live(ctx context.Context, t *testing.T) float64 {
	t.Helper()
	var v float64
	require.NoError(t, e.conn.QueryRow(ctx, `SELECT COALESCE(SUM(amount_usd_hr),0)::float8 FROM ladder_tranches WHERE config_id=$1 AND status IN ('scheduled','fired')`, e.cfg.ID).Scan(&v))
	return v
}

func (e *ladderB1Env) planAndTotal(ctx context.Context, t *testing.T, runID string) (ladderPlanJSONDTO, float64) {
	t.Helper()
	var raw []byte
	var total float64
	require.NoError(t, e.conn.QueryRow(ctx, `SELECT plan, total_hourly_commit::float8 FROM ladder_runs WHERE id=$1`, runID).Scan(&raw, &total))
	var dto ladderPlanJSONDTO
	require.NoError(t, json.Unmarshal(raw, &dto))
	return dto, total
}

var ladderB1TwoStep = []pkgladder.RampStep{{AfterDays: 0, Fraction: 0.5}, {AfterDays: 30, Fraction: 0.5}}

func TestLadderB1_ImmediateStepPersistedAndPlanMatchesRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := newLadderB1Env(ctx, t, ladderB1Conn(ctx, t), ladderB1TwoStep)

	runID := e.run(ctx, t, e.start)
	rows := e.rows(ctx, t, runID)
	require.Len(t, rows, 2)
	assert.InDelta(t, 5.0, rows[0].amount, 1e-9)
	assert.InDelta(t, 5.0, rows[1].amount, 1e-9)
	assert.True(t, rows[0].at.Equal(e.start), "immediate tranche scheduled_date must be the run start")
	assert.True(t, rows[1].at.Equal(e.start.AddDate(0, 0, 30)))
	assert.Equal(t, "scheduled", rows[0].status)

	// Plan JSON is display only and agrees with the persisted rows: the one
	// purchase action is the immediate step, total is the sum of both rows.
	plan, total := e.planAndTotal(ctx, t, runID)
	var purchases []ladderPlanJSONAction
	for _, a := range plan.Actions {
		if a.Action == string(pkgladder.ActionPurchase) {
			purchases = append(purchases, a)
		}
	}
	require.Len(t, purchases, 1)
	require.NotNil(t, purchases[0].AmountUSDHr)
	assert.InDelta(t, rows[0].amount, *purchases[0].AmountUSDHr, 1e-9)
	assert.InDelta(t, rows[0].amount+rows[1].amount, total, 1e-9)
	require.NotNil(t, plan.GapUSDHr)
	assert.InDelta(t, ladderB1Target, *plan.GapUSDHr, 1e-9)
}

func TestLadderB1_ExampleRampAfterDays0Fraction1_PersistsOneTranche(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The UI example ramp (frontend ladder.ts): after_days 0, fraction 1.0.
	e := newLadderB1Env(ctx, t, ladderB1Conn(ctx, t), []pkgladder.RampStep{{AfterDays: 0, Fraction: 1.0}})
	runID := e.run(ctx, t, e.start)
	rows := e.rows(ctx, t, runID)
	require.Len(t, rows, 1)
	assert.InDelta(t, ladderB1Target, rows[0].amount, 1e-9)
	assert.True(t, rows[0].at.Equal(e.start))
	plan, total := e.planAndTotal(ctx, t, runID)
	assert.InDelta(t, ladderB1Target, total, 1e-9)
	require.Len(t, plan.Actions, 1)
	assert.InDelta(t, ladderB1Target, *plan.Actions[0].AmountUSDHr, 1e-9)
}

// Grace-boundary table: run 2 follows run 1 (ramp [0:.5,30:.5], gap 10) by
// `after`. The immediate row (scheduled_date = run 1 start) is netted while
// scheduled_date > run2 - 24h and canceled at <=; a boundary row is counted
// or canceled, never neither. Netted: run 2 plans nothing. Canceled: only
// the 5 that is no longer in flight is re-planned.
func TestLadderB1_NextRunNetsImmediateRowWithinGrace(t *testing.T) {
	conn := ladderB1Conn(t.Context(), t)
	cases := []struct {
		name         string
		after        time.Duration
		wantNew      float64
		wantImmediat string
	}{
		{"1h", time.Hour, 0, string(pkgladder.TrancheStatusScheduled)},
		{"grace minus 1us", config.LadderFireGrace - time.Microsecond, 0, string(pkgladder.TrancheStatusScheduled)},
		{"exactly grace", config.LadderFireGrace, 5, string(pkgladder.TrancheStatusCancelled)},
		{"grace plus 1us", config.LadderFireGrace + time.Microsecond, 5, string(pkgladder.TrancheStatusCancelled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			e := newLadderB1Env(ctx, t, conn, ladderB1TwoStep)
			first := e.run(ctx, t, e.start)
			second := e.run(ctx, t, e.start.Add(tc.after))

			var sum float64
			for _, r := range e.rows(ctx, t, second) {
				sum += r.amount
			}
			assert.InDelta(t, tc.wantNew, sum, 1e-9)
			assert.Equal(t, tc.wantImmediat, e.rows(ctx, t, first)[0].status)
			assert.LessOrEqual(t, e.live(ctx, t), ladderB1Target+1e-6)
		})
	}
}

// Fixed point: re-running daily for 40 days (nothing fires: no executor) never
// leaves more live commitment than target-E, and ends at the target. Each ramp
// step is floored to the six-decimal column (#1182), never rounded up, so the
// fixed point can sit at most one micro-dollar per ramp step below the target
// (far under the provider's five-decimal precision) and never above it.
func TestLadderB1_DailyRerunNeverExceedsTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e := newLadderB1Env(ctx, t, ladderB1Conn(ctx, t), ladderB1TwoStep)
	for day := 0; day <= 40; day++ {
		e.run(ctx, t, e.start.Add(time.Duration(day)*24*time.Hour))
		live := e.live(ctx, t)
		require.LessOrEqual(t, live, ladderB1Target, "day %d over-plans: live=%v", day, live)
	}
	floorSlack := float64(len(ladderB1TwoStep))*1e-6 + 1e-9 // + float8 noise on the summed NUMERIC
	assert.GreaterOrEqual(t, e.live(ctx, t), ladderB1Target-floorSlack)
}

// completeBeforeStaleE completes a tranche inside GetLayerStates and then
// returns the stale E that predates the completion: the #843 race.
type completeBeforeStaleE struct {
	*fakeLadderCapability
	complete func()
}

func (c *completeBeforeStaleE) GetLayerStates(ctx context.Context, scope pkgladder.Scope) (map[pkgladder.LayerType]pkgladder.LayerState, error) {
	c.complete()
	return c.fakeLadderCapability.GetLayerStates(ctx, scope)
}

// A tranche that goes fired->completed between the E read and the locked
// in-flight read is in neither E (stale) nor the in-flight sum (not fired any
// more). Run 1 plans 10 as 5 immediate + 5 in 30 days; the immediate one is
// fired, then completes during run 2's E read. Run 2 must net both rows (10)
// and plan nothing; before the fix it re-planned the completed 5.
// FIXTURE capability; real store, real schema.
func TestLadderB1_TrancheCompletingBetweenEReadAndLockedSumIsNetted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := newLadderB1Env(ctx, t, ladderB1Conn(ctx, t), ladderB1TwoStep)
	first := e.run(ctx, t, e.start)
	immediate := e.rows(ctx, t, first)[0]
	require.InDelta(t, 5.0, immediate.amount, 1e-9)
	var trancheID string
	require.NoError(t, e.conn.QueryRow(ctx, `UPDATE ladder_tranches SET status='fired' WHERE run_id=$1 AND scheduled_date=$2 RETURNING id`, first, immediate.at).Scan(&trancheID))

	now2 := time.Now().UTC().Truncate(time.Microsecond) // captured before the E read, as the handler does
	e.cap2 = &completeBeforeStaleE{fakeLadderCapability: e.cap, complete: func() {
		moved, err := config.NewPostgresStore(e.conn).TransitionLadderTranche(ctx, trancheID, pkgladder.TrancheStatusFired, pkgladder.TrancheStatusCompleted, "commitment_visible")
		require.NoError(t, err)
		require.True(t, moved)
	}}
	second := e.run(ctx, t, now2)

	var sum float64
	for _, r := range e.rows(ctx, t, second) {
		sum += r.amount
	}
	assert.InDelta(t, 0, sum, 1e-9, "the completed tranche must be netted, not re-planned")
}
