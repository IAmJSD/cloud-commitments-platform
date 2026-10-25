//go:build integration

package database_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type queryLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *queryLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *queryLogBuffer) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := b.buf.String()
	b.buf.Reset()
	return text
}

func TestQueryLogging_RedactsBoundSecretsOnFailure(t *testing.T) {
	ctx := context.Background()
	// Empty, not migrated: the test creates the one table it needs.
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	var output queryLogBuffer
	previousOutput := logging.SetOutput(&output)
	previousLevel := logging.GetLevel()
	logging.SetLevelValue(logging.LevelDebug)
	t.Cleanup(func() {
		logging.SetOutput(previousOutput)
		logging.SetLevelValue(previousLevel)
	})

	_, err := container.DB.Exec(ctx, `CREATE TABLE query_logging_secrets (
		session_token text, password_hash text, approval_token text,
		accepted integer CHECK (accepted > 0))`)
	require.NoError(t, err)
	secrets := []string{strings.Repeat("a", 64), strings.Repeat("b", 60), strings.Repeat("c", 64)}
	for _, optIn := range []string{"", "true"} {
		t.Run("bind_parameters="+optIn, func(t *testing.T) {
			t.Setenv("DB_LOG_BIND_PARAMETERS", optIn)
			output.take()
			_, err := container.DB.Exec(ctx,
				`INSERT INTO query_logging_secrets VALUES ($1, $2, $3, $4)`,
				secrets[0], secrets[1], secrets[2], 0)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, "23514", pgErr.Code)
			for _, secret := range secrets {
				require.NotContains(t, pgErr.Error(), secret)
			}
			logged := output.take()
			assert.Contains(t, logged, "Query")
			assert.Contains(t, logged, pgErr.Error())
			for _, secret := range secrets {
				if optIn == "true" {
					assert.Contains(t, logged, secret)
				} else {
					assert.NotContains(t, logged, secret)
				}
			}
			if optIn == "true" {
				assert.Contains(t, logged, "args:")
			} else {
				assert.NotContains(t, logged, "args:")
			}
		})
	}
}
