package config

// store_postgres_rollback_test.go — regression tests for issue #57: a
// genuine failure of the deferred best-effort tx.Rollback must be logged,
// not silently discarded, while the benign pgx.ErrTxClosed (rollback after
// a successful Commit) must stay quiet.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureConfigLogs redirects the shared logger into a buffer at the given
// level for the duration of the test. Tests using it must not run in
// parallel.
func captureConfigLogs(t *testing.T, level logging.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := logging.SetOutput(&buf)
	prevLevel := logging.GetLevel()
	logging.SetLevelValue(level)
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetLevelValue(prevLevel)
	})
	return &buf
}

// rollbackSites drives each transaction site down its error path. pgxmock
// is primed with only Begin and Rollback, so the first statement inside the
// transaction fails as unexpected and the deferred rollback runs.
var rollbackSites = []struct {
	name string
	op   string
	run  func(ctx context.Context, s *PostgresStore) error
}{
	{"DeleteCloudAccount", "DeleteCloudAccount", func(ctx context.Context, s *PostgresStore) error {
		return s.DeleteCloudAccount(ctx, "11111111-1111-1111-1111-111111111111")
	}},
	{"SetPlanAccounts", "SetPlanAccounts", func(ctx context.Context, s *PostgresStore) error {
		return s.SetPlanAccounts(ctx, "22222222-2222-2222-2222-222222222222", nil)
	}},
	{"UpsertRecommendations", "UpsertRecommendations", func(ctx context.Context, s *PostgresStore) error {
		return s.UpsertRecommendations(ctx, time.Now(), nil, nil)
	}},
	{"WithTx", "WithTx", func(ctx context.Context, s *PostgresStore) error {
		return s.WithTx(ctx, func(pgx.Tx) error { return errors.New("user code failed") })
	}},
	{"withLadderConfigLock", "ladder config lock cfg-1", func(ctx context.Context, s *PostgresStore) error {
		return s.withLadderConfigLock(ctx, "cfg-1", func(pgx.Tx) error { return nil })
	}},
}

func TestTxRollbackFailureIsLogged(t *testing.T) {
	for _, tc := range rollbackSites {
		t.Run(tc.name, func(t *testing.T) {
			// Capture at Warn so a downgrade of the log level is caught.
			buf := captureConfigLogs(t, logging.LevelWarn)
			mock := newMock(t)
			mock.ExpectBegin()
			rbErr := errors.New("conn reset by peer")
			mock.ExpectRollback().WillReturnError(rbErr)

			err := tc.run(context.Background(), storeWith(mock))
			require.Error(t, err)
			assert.NotErrorIs(t, err, rbErr, "the rollback error must not replace the original error")
			require.NoError(t, mock.ExpectationsWereMet())

			out := buf.String()
			assert.Contains(t, out, "[WARN]", "rollback failure must be logged at WARN")
			assert.Contains(t, out, "conn reset by peer", "rollback failure must be logged")
			assert.Contains(t, out, tc.op, "log line must name the failing operation")
		})
	}
}

func TestTxRollbackAfterCloseIsQuiet(t *testing.T) {
	for _, tc := range rollbackSites {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureConfigLogs(t, logging.LevelDebug)
			mock := newMock(t)
			mock.ExpectBegin()
			mock.ExpectRollback().WillReturnError(pgx.ErrTxClosed)

			require.Error(t, tc.run(context.Background(), storeWith(mock)))
			require.NoError(t, mock.ExpectationsWereMet())
			assert.NotContains(t, buf.String(), "rollback",
				"pgx.ErrTxClosed is the benign post-Commit case and must not be logged")
		})
	}
}

// TestTxRollbackRunsOnDetachedContext covers the usual failure path: the
// request context is canceled while the transaction body runs. The rollback
// must not inherit that cancellation (it would fail with context.Canceled,
// leave the rollback undone and log noise); it runs on a detached context.
func TestTxRollbackRunsOnDetachedContext(t *testing.T) {
	buf := captureConfigLogs(t, logging.LevelDebug)
	mock := newMock(t)
	mock.ExpectBegin()
	// The delay makes pgxmock honor ctx: a canceled ctx returns
	// ctx.Err() at once, a live one waits and then succeeds.
	mock.ExpectRollback().WillDelayFor(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := storeWith(mock).WithTx(ctx, func(pgx.Tx) error {
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
	assert.NotContains(t, buf.String(), "rollback",
		"rollback on a detached context must succeed and log nothing")
}
