//go:build integration

package config

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestPostgresStoreDB_ReserveRIExchange_SerializesIndependentStores(t *testing.T) {
	dsn := riExchangeContainerDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	newPool := func() (*pgxpool.Pool, error) {
		cfg, parseErr := pgxpool.ParseConfig(dsn)
		if parseErr != nil {
			return nil, parseErr
		}
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, setErr := conn.Exec(ctx, "SET TIME ZONE 'America/Los_Angeles'")
			return setErr
		}
		return pgxpool.NewWithConfig(ctx, cfg)
	}
	poolA, err := newPool()
	require.NoError(t, err)
	defer poolA.Close()
	poolB, err := newPool()
	require.NoError(t, err)
	defer poolB.Close()
	storeA := &PostgresStore{db: poolA}
	storeB := &PostgresStore{db: poolB}
	beforeBoundaryRow, err := storeA.GetRIExchangeDailySpend(ctx, time.Now().UTC())
	require.NoError(t, err)
	beforeMicros, err := riExchangeMicros(beforeBoundaryRow)
	require.NoError(t, err)
	utcOneAM := time.Now().UTC().Truncate(24 * time.Hour).Add(time.Hour)
	boundaryRow := &RIExchangeRecord{AccountID: "444444444444", Region: "us-east-1", SourceRIIDs: []string{"ri-boundary"},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "1.000000", Status: "completed", Mode: "auto", CompletedAt: &utcOneAM}
	require.NoError(t, storeA.SaveRIExchangeRecord(ctx, boundaryRow))
	baseline, err := storeA.GetRIExchangeDailySpend(ctx, time.Now().UTC())
	require.NoError(t, err)
	baselineMicros, err := riExchangeMicros(baseline)
	require.NoError(t, err)
	require.Equal(t, 0, new(big.Int).Sub(baselineMicros, beforeMicros).Cmp(big.NewInt(1_000_000)), "UTC 01:00 exchange must count even when session timezone is Los Angeles")
	cap := riExchangeUSD(new(big.Int).Add(baselineMicros, big.NewInt(1_000_000_000)))

	start := make(chan struct{})
	type reservation struct {
		record *RIExchangeRecord
		amount string
		err    error
	}
	results := make(chan reservation, 2)
	var workers sync.WaitGroup
	for i, store := range []*PostgresStore{storeA, storeB} {
		workers.Add(1)
		go func(i int, store *PostgresStore) {
			defer workers.Done()
			<-start
			record := &RIExchangeRecord{
				AccountID: []string{"111111111111", "222222222222"}[i], Region: []string{"us-east-1", "eu-west-1"}[i],
				SourceRIIDs: []string{"ri-" + string(rune('A'+i))}, SourceInstanceType: "m5.large", SourceCount: 1,
				TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
				PaymentDue: "900.000000", Status: "processing", Mode: "auto",
			}
			amount, reserveErr := store.ReserveRIExchange(ctx, record, cap, "1000.000000")
			results <- reservation{record: record, amount: amount, err: reserveErr}
		}(i, store)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner *RIExchangeRecord
	for result := range results {
		if result.err == nil {
			require.Nil(t, winner, "two stores reserved against the same daily headroom")
			winner = result.record
			require.Equal(t, "1000.000000", result.amount)
			continue
		}
		require.ErrorContains(t, result.err, "daily cap exceeded")
	}
	require.NotNil(t, winner)
	require.NotEmpty(t, winner.ID)

	var status, payment string
	require.NoError(t, poolB.QueryRow(ctx, "SELECT status, payment_due::text FROM ri_exchange_history WHERE id = $1", winner.ID).Scan(&status, &payment))
	require.Equal(t, "processing", status)
	require.Equal(t, "1000.000000", payment)
	require.NoError(t, storeA.CompleteRIExchangeWithPayment(ctx, winner.ID, "exchange-accepted", "950.000000"))
	require.NoError(t, poolB.QueryRow(ctx, "SELECT status, payment_due::text FROM ri_exchange_history WHERE id = $1", winner.ID).Scan(&status, &payment))
	require.Equal(t, "completed", status)
	require.Equal(t, "950.000000", payment)
	spend, err := storeB.GetRIExchangeDailySpend(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, riExchangeUSD(new(big.Int).Add(baselineMicros, big.NewInt(950_000_000))), spend)

	third := &RIExchangeRecord{AccountID: "333333333333", Region: "ap-south-1", SourceRIIDs: []string{"ri-C"},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "50.000000", Status: "processing", Mode: "auto"}
	remaining, err := storeB.ReserveRIExchange(ctx, third, cap, "1000.000000")
	require.NoError(t, err)
	require.Equal(t, "50.000000", remaining)

	var rowsBefore int
	require.NoError(t, poolA.QueryRow(ctx, "SELECT COUNT(*) FROM ri_exchange_history").Scan(&rowsBefore))
	lockTx, err := poolA.Begin(ctx)
	require.NoError(t, err)
	defer lockTx.Rollback(ctx)
	_, err = lockTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", riExchangeDailySpendLockKey)
	require.NoError(t, err)
	probe := &RIExchangeRecord{AccountID: "777777777777", Region: "us-west-2", SourceRIIDs: []string{"ri-lock-probe"},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "1.000000", Status: "processing", Mode: "auto"}
	probeResult := make(chan error, 1)
	go func() {
		_, reserveErr := storeB.ReserveRIExchange(ctx, probe, riExchangeUSD(new(big.Int).Add(baselineMicros, big.NewInt(2_000_000_000))), "1.000000")
		probeResult <- reserveErr
	}()
	observedWait := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		var waiting bool
		require.NoError(t, poolA.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'
			AND query LIKE 'SELECT pg_advisory_xact_lock%')`).Scan(&waiting))
		if waiting {
			observedWait = true
			break
		}
		select {
		case reserveErr := <-probeResult:
			t.Fatalf("reservation returned before lock release: %v", reserveErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	require.True(t, observedWait, "reservation must wait on the same global advisory lock")
	var rowsDuring int
	require.NoError(t, poolA.QueryRow(ctx, "SELECT COUNT(*) FROM ri_exchange_history").Scan(&rowsDuring))
	require.Equal(t, rowsBefore, rowsDuring)
	require.NoError(t, lockTx.Commit(ctx))
	select {
	case reserveErr := <-probeResult:
		require.NoError(t, reserveErr)
	case <-time.After(3 * time.Second):
		t.Fatal("reservation did not proceed after global advisory lock release")
	}
}
