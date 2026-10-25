//go:build integration

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestEnsureDB_IsLambdaSingleSourceOfTruth extends the issue #110 regression
// to the post-DB wiring: after ensureDB rebuilds the scheduler and API handler,
// both must still follow ApplicationConfig.IsLambda while the environment says
// the opposite.
func TestEnsureDB_IsLambdaSingleSourceOfTruth(t *testing.T) {
	for _, key := range []string{
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ARN", "CREDENTIAL_ENCRYPTION_KEY_SECRET_NAME",
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ID", "CREDENTIAL_ENCRYPTION_ALLOW_DEV_KEY",
		"CUDLY_SIGNING_KEY_ID", "CUDLY_SIGNING_KEY_VAULT_URL", "CUDLY_SIGNING_KEY_NAME",
		"CUDLY_SIGNING_KEY_RESOURCE", "AWS_PROFILE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", strings.Repeat("12", 32))
	t.Setenv("CUDLY_ISSUER_URL", "https://islambda.example.test")
	t.Setenv("CUDLY_SOURCE_CLOUD", "aws")
	t.Setenv("SCHEDULED_TASK_AUTH_MODE", "disabled")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "local-islambda-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local-islambda-test")
	t.Setenv("AWS_REGION", "us-east-1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")

	for _, cfgIsLambda := range []bool{true, false} {
		name := "config not Lambda, environment Lambda"
		if cfgIsLambda {
			name = "config Lambda, environment not Lambda"
		}
		t.Run(name, func(t *testing.T) {
			setLambdaEnv(t, !cfgIsLambda)
			cfg := ApplicationConfig{
				IsLambda: cfgIsLambda, DashboardURL: "https://islambda.example.test",
				DefaultTerm: 3, DefaultCoverage: 80,
			}
			app, appErr := NewApplicationFromDeps(ctx, cfg, ExternalDeps{
				DBConfig: pg.Config, EmailSender: &noopEmailSender{},
			})
			require.NoError(t, appErr)
			t.Cleanup(func() { require.NoError(t, app.Close()) })

			require.NoError(t, app.ensureDB(ctx))
			require.NotNil(t, app.DB, "ensureDB must have rebuilt the post-DB wiring")
			assertLambdaModeWiring(t, app, cfgIsLambda)
		})
	}
}
