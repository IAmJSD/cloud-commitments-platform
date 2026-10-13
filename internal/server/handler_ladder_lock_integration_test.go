//go:build integration

package server

// Real PostgreSQL test for the ladder persist lock: two overlapping
// executeLadderRun calls for one config must plan the gap once. The ladder
// capability is a fixture (labeled FIXTURE in newLadderB1Env); the store, the
// lock and the netting query are real. Waiting is observed in pg_locks.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookedLadderStore wraps the real store and holds the FIRST plan builder
// inside the locked transaction until released.
type hookedLadderStore struct {
	*config.PostgresStore
	calls   atomic.Int32
	entered chan struct{} // closed when the first builder is running under the lock
	second  chan struct{} // closed if a second builder runs while the first is held
	gate    chan struct{}
}

func (h *hookedLadderStore) PlanAndSaveLadderRun(ctx context.Context, configID string, asOf time.Time, build config.LadderPlanBuilder) (*config.LadderRunDB, error) {
	return h.PostgresStore.PlanAndSaveLadderRun(ctx, configID, asOf, func(inFlight float64) (*config.LadderRunDB, []config.LadderTrancheDB, error) {
		if h.calls.Add(1) == 1 {
			close(h.entered)
			<-h.gate
		} else {
			select {
			case <-h.second:
			default:
				close(h.second)
			}
		}
		return build(inFlight)
	})
}

// waitersOnLadderLock counts sessions blocked on the config's ladder lock key
// (cudly:ladder-config:<id>, split across pg_locks classid/objid).
func waitersOnLadderLock(ctx context.Context, t *testing.T, e *ladderB1Env) int {
	t.Helper()
	var n int
	require.NoError(t, e.conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks l, (SELECT hashtextextended('cudly:ladder-config:' || $1::text, 0) AS k) h
		WHERE l.locktype = 'advisory' AND l.objsubid = 1 AND NOT l.granted
		  AND l.classid = ((h.k >> 32) & 4294967295)::oid
		  AND l.objid = (h.k & 4294967295)::oid`, e.cfg.ID).Scan(&n))
	return n
}

func TestLadderLock_ConcurrentRunsPlanTheGapOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn := ladderB1Conn(ctx, t)
	e := newLadderB1Env(ctx, t, conn, ladderB1TwoStep)
	hook := &hookedLadderStore{
		PostgresStore: config.NewPostgresStore(conn),
		entered:       make(chan struct{}), second: make(chan struct{}), gate: make(chan struct{}),
	}
	e.app.Config = hook

	runOnce := func() chan error {
		errc := make(chan error, 1)
		go func() {
			errc <- e.app.executeLadderRun(ctx, e.cfg, e.cap, "123456789012", pkgladder.Term1Year, pkgladder.PaymentNoUpfront, e.start)
		}()
		return errc
	}
	first := runOnce()
	select {
	case <-hook.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("first run never reached the locked plan builder")
	}
	second := runOnce()

	// The second run must be parked on the lock, not planning alongside the first.
	require.Eventually(t, func() bool {
		select {
		case <-hook.second:
			return true // lock failed to serialize: stop waiting
		default:
		}
		return waitersOnLadderLock(ctx, t, e) == 1
	}, 20*time.Second, 20*time.Millisecond)
	select {
	case <-hook.second:
		t.Error("second run planned while the first still held the lock")
	default:
	}

	close(hook.gate)
	require.NoError(t, <-first)
	require.NoError(t, <-second)

	// Target is 10: the second run must have netted the first's rows (gap 0).
	assert.InDelta(t, 10.0, e.live(ctx, t), 1e-9, "two concurrent runs must not both plan the gap")
}
