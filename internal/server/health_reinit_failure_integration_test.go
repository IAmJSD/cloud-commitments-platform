//go:build integration

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealth_ConfigStoreAfterFailedReinitialization is the #339 (A06-009)
// regression test on the real path: ensureDB connects to a live Postgres,
// reinitializeAfterConnect sets app.Config and then fails (here at the AWS
// config load, via a profile that does not exist), and ensureDB closes the
// pool. The config_store check used to report "healthy" over that closed pool.
func TestHealth_ConfigStoreAfterFailedReinitialization(t *testing.T) {
	for _, key := range []string{
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ARN", "CREDENTIAL_ENCRYPTION_KEY_SECRET_NAME",
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ID", "CREDENTIAL_ENCRYPTION_ALLOW_DEV_KEY",
		"CUDLY_SIGNING_KEY_ID", "CUDLY_SIGNING_KEY_VAULT_URL", "CUDLY_SIGNING_KEY_NAME",
		"CUDLY_SIGNING_KEY_RESOURCE", "AWS_LAMBDA_RUNTIME_API",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", strings.Repeat("12", 32))
	t.Setenv("CUDLY_ISSUER_URL", "https://health-339.example.test")
	t.Setenv("CUDLY_SOURCE_CLOUD", "aws")
	t.Setenv("SCHEDULED_TASK_AUTH_MODE", "disabled")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	// LoadDefaultConfig fails on a named profile missing from the config file,
	// after reinitializeAfterConnect has already published app.Config.
	t.Setenv("AWS_PROFILE", "issue-339-missing-profile")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })

	app, err := NewApplicationFromDeps(ctx, ApplicationConfig{DashboardURL: "https://health-339.example.test"},
		ExternalDeps{DBConfig: pg.Config, EmailSender: &noopEmailSender{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, app.Close()) })

	err = app.ensureDB(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to reinitialize after DB connect")
	require.NotNil(t, app.Config, "the failure path leaves app.Config over the closed pool")
	require.Nil(t, app.DB)
	require.False(t, app.dbConnected)

	health := app.collectHealth(ctx)
	assert.Equal(t, "unhealthy", health.Checks["config_store"].Status,
		"config_store must not report healthy over a store whose pool ensureDB closed")
	assert.Equal(t, "degraded", health.Status)
	assert.False(t, health.ready())
}
