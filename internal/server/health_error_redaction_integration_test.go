//go:build integration

package server

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckConfigStore_DBFailureHidesRawError drives the config-store check
// against a real pgx pool whose ping fails, and pins issue #111: the result
// carries a fixed message and the db_unreachable reason, not the pgx error.
func TestCheckConfigStore_DBFailureHidesRawError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pg := testhelpers.RequirePostgresContainer(ctx, t)

	conn, err := database.NewConnection(ctx, pg.Config, nil)
	require.NoError(t, err)
	conn.Close()
	require.Error(t, conn.HealthCheck(ctx), "a closed pool must fail its health check")

	// Through collectHealth rather than checkConfigStore directly, so the
	// test does not depend on that helper's parameter list.
	app := &Application{Config: &mockConfigStoreForHealth{}, DB: conn, Auth: createHealthyAuthService()}
	got := app.collectHealth(ctx)
	assert.Equal(t, "degraded", got.Status)
	assert.Equal(t, CheckResult{Status: "unhealthy", Message: "Database health check failed", Reason: healthReasonDBUnreachable}, got.Checks["config_store"])
}
