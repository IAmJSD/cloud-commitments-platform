//go:build integration

package config

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Real-PostgreSQL coverage for #656 (approved exchanges reserve the full ceiling
// under the shared lock) and #657 (in-flight holds survive UTC rollover). Each
// test gets its own PostgreSQL container, like the AUTO reservation test, so it
// runs in CI instead of skipping on a missing environment variable (#724).

type riExchangeIntegrationEnv struct {
	ctx            context.Context
	poolA, poolB   *pgxpool.Pool
	storeA, storeB *PostgresStore
}

// riExchangeContainerDSN starts a PostgreSQL container dedicated to t, applies
// every migration to it and returns its connection string, so callers can open
// as many independent pools as the race under test needs. It skips only when
// Docker is unavailable (RequirePostgresContainer's rule); a container that will
// not start or a migration that will not apply fails the test (#724, #1597).
// Setup gets its own deadline so a slow container start does not eat into the
// test body's.
func riExchangeContainerDSN(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	require.NoError(t,
		migrations.RunMigrations(ctx, container.DB.Pool(), getTestMigrationsPath(), "", ""),
		"migrations failed to apply to a fresh database")
	return container.Config.DSN("")
}

func newRIExchangeIntegrationEnv(t *testing.T) *riExchangeIntegrationEnv {
	t.Helper()
	dsn := riExchangeContainerDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	poolA, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(poolA.Close)
	poolB, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(poolB.Close)
	return &riExchangeIntegrationEnv{ctx: ctx, poolA: poolA, poolB: poolB,
		storeA: &PostgresStore{db: poolA}, storeB: &PostgresStore{db: poolB}}
}

// spend returns the daily spend for date as micros.
func (e *riExchangeIntegrationEnv) spend(t *testing.T, date time.Time) *big.Int {
	t.Helper()
	text, err := e.storeA.GetRIExchangeDailySpend(e.ctx, date)
	require.NoError(t, err)
	micros, err := riExchangeMicros(text)
	require.NoError(t, err)
	return micros
}

// testAccountID renders a 12-digit account id without a literal that the
// git-secrets AWS account-id pattern would flag.
func testAccountID(n int64) string { return fmt.Sprintf("%012d", n) }

func usdMicros(dollars int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(dollars), riExchangeMicroScale)
}

// capAbove returns a decimal cap that leaves exactly $1000 of headroom above the
// spend already on the books at date.
func (e *riExchangeIntegrationEnv) capAbove(t *testing.T, date time.Time) string {
	t.Helper()
	return riExchangeUSD(new(big.Int).Add(e.spend(t, date), usdMicros(1000)))
}

// processing inserts a processing row holding payment dollars, as a manual
// approval would after the pending->processing transition.
func (e *riExchangeIntegrationEnv) processing(t *testing.T, mode, account string, payment int64, updatedAt time.Time) *RIExchangeRecord {
	t.Helper()
	record := &RIExchangeRecord{AccountID: account, Region: "us-east-1", SourceRIIDs: []string{"ri-" + account},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: riExchangeUSD(usdMicros(payment)), Status: "processing", Mode: mode}
	require.NoError(t, e.storeA.SaveRIExchangeRecord(e.ctx, record))
	e.setUpdatedAt(t, record.ID, updatedAt)
	return record
}

// setUpdatedAt back-dates a row. The ri_exchange_updated_at trigger rewrites
// updated_at on every UPDATE, so it is bypassed for this one statement
// (session_replication_role needs a superuser, which the container's user is).
func (e *riExchangeIntegrationEnv) setUpdatedAt(t *testing.T, id string, updatedAt time.Time) {
	t.Helper()
	tx, err := e.poolA.Begin(e.ctx)
	require.NoError(t, err)
	defer tx.Rollback(e.ctx)
	_, err = tx.Exec(e.ctx, "SET LOCAL session_replication_role = replica")
	require.NoError(t, err)
	tag, err := tx.Exec(e.ctx, "UPDATE ri_exchange_history SET updated_at = $2 WHERE id = $1", id, updatedAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
	require.NoError(t, tx.Commit(e.ctx))
	var got time.Time
	require.NoError(t, e.poolA.QueryRow(e.ctx, "SELECT updated_at FROM ri_exchange_history WHERE id = $1", id).Scan(&got))
	require.WithinDuration(t, updatedAt, got, time.Millisecond, "back-dating updated_at did not stick")
}

// release marks a test row failed so it stops holding headroom for later runs.
func (e *riExchangeIntegrationEnv) release(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, e.storeA.FailRIExchange(e.ctx, id, "test cleanup"))
	}
}

func TestPostgresStoreDB_ReserveApprovedRIExchange_ConcurrentApprovalsStayWithinCap(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	cap := env.capAbove(t, now)
	a := env.processing(t, "manual", "810000000001", 100, now)
	b := env.processing(t, "manual", "810000000002", 100, now)
	defer env.release(t, a.ID, b.ID)

	type outcome struct {
		ceiling string
		err     error
	}
	results := make([]outcome, 2)
	// Deterministic interleave: hold the global lock from a third transaction so both
	// approvals are provably in flight at once, then release it. Without the advisory
	// lock in the reservation path neither approval would park, so the wait check fails.
	lockTx, err := env.poolA.Begin(env.ctx)
	require.NoError(t, err)
	defer lockTx.Rollback(env.ctx)
	_, err = lockTx.Exec(env.ctx, "SELECT pg_advisory_xact_lock($1)", riExchangeDailySpendLockKey)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i, run := range []struct {
		store *PostgresStore
		id    string
	}{{env.storeA, a.ID}, {env.storeB, b.ID}} {
		wg.Add(1)
		go func(i int, store *PostgresStore, id string) {
			defer wg.Done()
			ceiling, reserveErr := store.ReserveApprovedRIExchange(env.ctx, id, cap, "800.000000")
			results[i] = outcome{ceiling, reserveErr}
		}(i, run.store, run.id)
	}
	waiters := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		require.NoError(t, env.poolA.QueryRow(env.ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND wait_event = 'advisory' AND query LIKE 'SELECT pg_advisory_xact_lock%'`).Scan(&waiters))
		if waiters >= 2 {
			break
		}
	}
	require.Equal(t, 2, waiters, "both approvals must be parked on the global advisory lock at the same time")
	for _, id := range []string{a.ID, b.ID} {
		var stored string
		require.NoError(t, env.poolA.QueryRow(env.ctx, "SELECT payment_due::text FROM ri_exchange_history WHERE id = $1", id).Scan(&stored))
		require.Equal(t, "100.000000", stored, "no approval may reserve before the lock is released")
	}
	require.NoError(t, lockTx.Commit(env.ctx))
	wg.Wait()

	reserved := new(big.Int)
	for _, r := range results {
		require.NoError(t, r.err)
		micros, parseErr := riExchangeMicros(r.ceiling)
		require.NoError(t, parseErr)
		reserved.Add(reserved, micros)
	}
	require.Equal(t, 0, reserved.Cmp(usdMicros(1000)),
		"two concurrent approvals must divide, not double, the headroom: %s + %s", results[0].ceiling, results[1].ceiling)

	// The headroom is fully reserved, so a later approval is refused.
	c := env.processing(t, "manual", "810000000003", 1, now)
	defer env.release(t, c.ID)
	_, err = env.storeA.ReserveApprovedRIExchange(env.ctx, c.ID, cap, "800.000000")
	require.ErrorContains(t, err, "daily cap exceeded")

	// Each approval settles at its ceiling in the worst case; the day lands exactly on the cap
	// (c's refused $1 row is still processing until the handler fails it).
	for i, id := range []string{a.ID, b.ID} {
		require.NoError(t, env.storeA.CompleteRIExchangeWithPayment(env.ctx, id, "exchange-"+string(rune('a'+i)), results[i].ceiling))
	}
	capMicros, err := riExchangeMicros(cap)
	require.NoError(t, err)
	require.Equal(t, 0, env.spend(t, now).Cmp(new(big.Int).Add(capMicros, usdMicros(1))))
}

func TestPostgresStoreDB_ReserveApprovedRIExchange_DoesNotCountOwnInitialQuote(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	cap := env.capAbove(t, now)
	row := env.processing(t, "manual", "820000000001", 600, now)
	defer env.release(t, row.ID)

	ceiling, err := env.storeA.ReserveApprovedRIExchange(env.ctx, row.ID, cap, "1000.000000")
	require.NoError(t, err, "a $600 approval under a $1000 cap must not be rejected for counting itself twice")
	require.Equal(t, "1000.000000", ceiling)

	var stored string
	require.NoError(t, env.poolB.QueryRow(env.ctx, "SELECT payment_due::text FROM ri_exchange_history WHERE id = $1", row.ID).Scan(&stored))
	require.Equal(t, "1000.000000", stored, "the full ceiling is what the row reserves while it executes")
}

func TestPostgresStoreDB_ReserveApprovedRIExchange_RejectsRowThatIsNotProcessing(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	row := env.processing(t, "manual", "830000000001", 10, time.Now().UTC())
	env.release(t, row.ID)

	_, err := env.storeA.ReserveApprovedRIExchange(env.ctx, row.ID, env.capAbove(t, time.Now().UTC()), "1000.000000")
	require.ErrorContains(t, err, "is not processing")
}

func TestPostgresStoreDB_ReserveApprovedRIExchange_AutoAndApprovalShareHeadroom(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	cap := env.capAbove(t, now)
	approved := env.processing(t, "manual", "840000000001", 100, now)
	auto := &RIExchangeRecord{AccountID: testAccountID(840000000002), Region: "eu-west-1", SourceRIIDs: []string{"ri-auto-interleave"},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "100.000000", Status: "processing", Mode: "auto"}
	defer func() {
		env.release(t, approved.ID)
		if auto.ID != "" {
			env.release(t, auto.ID)
		}
	}()

	var approvedCeiling, autoCeiling string
	var approvedErr, autoErr error
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		approvedCeiling, approvedErr = env.storeA.ReserveApprovedRIExchange(env.ctx, approved.ID, cap, "800.000000")
	}()
	go func() {
		defer wg.Done()
		<-start
		autoCeiling, autoErr = env.storeB.ReserveRIExchange(env.ctx, auto, cap, "800.000000")
	}()
	close(start)
	wg.Wait()
	require.NoError(t, approvedErr)
	require.NoError(t, autoErr)

	approvedMicros, err := riExchangeMicros(approvedCeiling)
	require.NoError(t, err)
	autoMicros, err := riExchangeMicros(autoCeiling)
	require.NoError(t, err)
	require.Equal(t, 0, new(big.Int).Add(approvedMicros, autoMicros).Cmp(usdMicros(1000)),
		"AUTO and approval reservations must divide, not double, the headroom: %s + %s", approvedCeiling, autoCeiling)
}

func TestPostgresStoreDB_DailySpend_InFlightReservationSurvivesUTCRollover(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	dayB := dayStart.Add(36 * time.Hour) // frozen "next day" read; the query takes the date explicitly
	baselineB := env.spend(t, dayB)

	hold := env.processing(t, "auto", "850000000001", 1000, dayStart.Add(time.Minute))
	done := env.processing(t, "auto", "850000000002", 500, dayStart.Add(time.Minute))
	defer env.release(t, hold.ID, done.ID)
	require.NoError(t, env.storeA.CompleteRIExchangeWithPayment(env.ctx, done.ID, "exchange-day-a", "500.000000"))
	_, err := env.poolA.Exec(env.ctx, "UPDATE ri_exchange_history SET completed_at = $2 WHERE id = $1", done.ID, dayStart.Add(2*time.Minute))
	require.NoError(t, err)

	got := new(big.Int).Sub(env.spend(t, dayB), baselineB)
	require.Equal(t, 0, got.Cmp(usdMicros(1000)),
		"day B must still count the unsettled day-A hold ($1000) and not the completed day-A spend ($500); delta was %s", riExchangeUSD(got))
}

func TestPostgresStoreDB_DailySpend_CompletedRowsFollowAcceptanceDayBoundary(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	dayB := dayStart.Add(24 * time.Hour)
	baselineA := env.spend(t, dayStart.Add(time.Hour))
	baselineB := env.spend(t, dayB.Add(time.Hour))

	lastMicroOfA := env.processing(t, "auto", "860000000001", 7, dayStart)
	firstInstantOfB := env.processing(t, "auto", "860000000002", 11, dayStart)
	defer env.release(t, lastMicroOfA.ID, firstInstantOfB.ID)
	require.NoError(t, env.storeA.CompleteRIExchangeWithPayment(env.ctx, lastMicroOfA.ID, "exchange-edge-a", "7.000000"))
	require.NoError(t, env.storeA.CompleteRIExchangeWithPayment(env.ctx, firstInstantOfB.ID, "exchange-edge-b", "11.000000"))
	_, err := env.poolA.Exec(env.ctx, "UPDATE ri_exchange_history SET completed_at = $2 WHERE id = $1", lastMicroOfA.ID, dayB.Add(-time.Microsecond))
	require.NoError(t, err)
	_, err = env.poolA.Exec(env.ctx, "UPDATE ri_exchange_history SET completed_at = $2 WHERE id = $1", firstInstantOfB.ID, dayB)
	require.NoError(t, err)

	deltaA := new(big.Int).Sub(env.spend(t, dayStart.Add(time.Hour)), baselineA)
	deltaB := new(big.Int).Sub(env.spend(t, dayB.Add(time.Hour)), baselineB)
	require.Equal(t, 0, deltaA.Cmp(usdMicros(7)), "00:00:00.999999 belongs to day A only; delta %s", riExchangeUSD(deltaA))
	require.Equal(t, 0, deltaB.Cmp(usdMicros(11)), "exactly 00:00:00 belongs to day B only; delta %s", riExchangeUSD(deltaB))
}

func TestPostgresStoreDB_ReserveRIExchange_PriorDayInFlightHoldReducesHeadroom(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	cap := env.capAbove(t, now)
	hold := env.processing(t, "auto", "870000000001", 1000, now.Add(-30*time.Hour))
	defer env.release(t, hold.ID)

	second := &RIExchangeRecord{AccountID: testAccountID(870000000002), Region: "us-east-1", SourceRIIDs: []string{"ri-rollover-second"},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "1.000000", Status: "processing", Mode: "auto"}
	_, err := env.storeA.ReserveRIExchange(env.ctx, second, cap, "1000.000000")
	require.ErrorContains(t, err, "daily cap exceeded", "a reservation still executing from yesterday must keep counting today")
	require.Empty(t, second.ID)

	// Once the hold settles, only its accepted amount counts, and only on its acceptance day.
	require.NoError(t, env.storeA.CompleteRIExchangeWithPayment(env.ctx, hold.ID, "exchange-rollover", "950.000000"))
	_, err = env.poolA.Exec(env.ctx, "UPDATE ri_exchange_history SET completed_at = $2 WHERE id = $1", hold.ID, now.Add(-30*time.Hour))
	require.NoError(t, err)
	ceiling, err := env.storeA.ReserveRIExchange(env.ctx, second, cap, "1000.000000")
	require.NoError(t, err)
	require.Equal(t, "1000.000000", ceiling)
	env.release(t, second.ID)
}
