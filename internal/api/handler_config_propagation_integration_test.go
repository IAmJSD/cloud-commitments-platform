//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestUpdateConfigPropagatesOnlyChangedDefaults is the issue #225 scenario
// against Postgres: a global-defaults PUT must keep each service's own term,
// payment and ramp, and a failed propagation must not save the global change.
func TestUpdateConfigPropagatesOnlyChangedDefaults(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)

	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	global.EnabledProviders = []string{"aws"}
	global.DefaultTerm, global.DefaultPayment, global.DefaultCoverage = 1, "no-upfront", 80
	global.DefaultRampSchedule = config.RampImmediate
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	require.NoError(t, store.SaveServiceConfig(ctx, &config.ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true, Term: 3, Payment: "all-upfront", Coverage: 90, RampSchedule: config.RampWeekly25Pct}))
	require.NoError(t, store.SaveServiceConfig(ctx, &config.ServiceConfig{Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "partial-upfront", Coverage: 60, RampSchedule: config.RampMonthly10Pct}))

	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	mockAuth.grantAdmin()
	handler := &Handler{config: store, auth: mockAuth}
	put := func(body string) error {
		_, putErr := handler.updateConfig(ctx, &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
			Body:    body,
		})
		return putErr
	}
	service := func(name string) config.ServiceConfig {
		svc, getErr := store.GetServiceConfig(ctx, "aws", name)
		require.NoError(t, getErr)
		return *svc
	}

	require.NoError(t, put(`{"default_coverage": 70}`))
	ec2, rds := service("ec2"), service("rds")
	require.Equal(t, []any{3, "all-upfront", 70.0, config.RampWeekly25Pct}, []any{ec2.Term, ec2.Payment, ec2.Coverage, ec2.RampSchedule})
	require.Equal(t, []any{1, "partial-upfront", 70.0, config.RampMonthly10Pct}, []any{rds.Term, rds.Payment, rds.Coverage, rds.RampSchedule})

	// Both writes must share one transaction, or a failed commit could leave
	// the services rewritten while the global change rolls back.
	var globalXmin string
	require.NoError(t, pg.DB.Pool().QueryRow(ctx, `SELECT xmin::text FROM global_config`).Scan(&globalXmin))
	rows, err := pg.DB.Pool().Query(ctx, `SELECT DISTINCT xmin::text FROM service_configs WHERE coverage = 70`)
	require.NoError(t, err)
	serviceXmins, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{globalXmin}, serviceXmins, "propagation must run in the global config transaction")

	// The recommendations lookback control round-trips the whole config, so
	// every default key is present but none changed.
	current, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	current.RecommendationsLookbackDays = 30
	roundTrip, err := json.Marshal(current)
	require.NoError(t, err)
	require.NoError(t, put(string(roundTrip)))
	require.Equal(t, ec2, service("ec2"))
	require.Equal(t, rds, service("rds"))

	// A partial PUT of an unrelated key (#539) leaves the stored defaults and
	// every service row untouched.
	defaultsBefore, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.NoError(t, put(`{"notification_email": "ops@example.com"}`))
	defaultsAfter, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, []any{defaultsBefore.DefaultTerm, defaultsBefore.DefaultPayment, defaultsBefore.DefaultCoverage},
		[]any{defaultsAfter.DefaultTerm, defaultsAfter.DefaultPayment, defaultsAfter.DefaultCoverage})
	require.Equal(t, "ops@example.com", *defaultsAfter.NotificationEmail)
	require.Equal(t, ec2, service("ec2"))
	require.Equal(t, rds, service("rds"))

	_, err = pg.DB.Pool().Exec(ctx, `
		CREATE FUNCTION reject_service_config_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'service_configs update rejected'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_service_config_update BEFORE UPDATE ON service_configs
		FOR EACH ROW EXECUTE FUNCTION reject_service_config_update();`)
	require.NoError(t, err)
	require.ErrorContains(t, put(`{"default_term": 3}`), "failed to propagate global defaults")
	after, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, after.DefaultTerm, "global change must roll back with the failed propagation")
	require.Equal(t, ec2, service("ec2"))
}

// TestUpdateConfigPaymentDefaultSkipsAzureAndGCP: the global default payment is
// an AWS token, so changing it must not rewrite Azure or GCP rows (an Azure
// monthly row would turn upfront; a GCP row would fail validation forever).
func TestUpdateConfigPaymentDefaultSkipsAzureAndGCP(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)

	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	global.EnabledProviders = []string{"aws", "azure", "gcp"}
	global.DefaultTerm, global.DefaultPayment, global.DefaultCoverage = 1, "no-upfront", 80
	global.DefaultRampSchedule = config.RampImmediate
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	for _, svc := range []config.ServiceConfig{
		{Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80, RampSchedule: config.RampImmediate},
		{Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80, RampSchedule: config.RampImmediate},
		{Provider: "gcp", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80, RampSchedule: config.RampImmediate},
	} {
		svc := svc
		require.NoError(t, store.SaveServiceConfig(ctx, &svc))
	}

	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	mockAuth.grantAdmin()
	handler := &Handler{config: store, auth: mockAuth}
	headers := map[string]string{"Authorization": "Bearer admin-token"}

	_, err = handler.updateConfig(ctx, &events.LambdaFunctionURLRequest{Headers: headers, Body: `{"default_payment": "all-upfront"}`})
	require.NoError(t, err)

	payment := func(provider string) string {
		name := "compute"
		if provider == "aws" {
			name = "ec2"
		}
		svc, getErr := store.GetServiceConfig(ctx, provider, name)
		require.NoError(t, getErr)
		return svc.Payment
	}
	require.Equal(t, "all-upfront", payment("aws"))
	require.Equal(t, "monthly", payment("azure"))
	require.Equal(t, "monthly", payment("gcp"))

	_, err = handler.updateServiceConfig(ctx, &events.LambdaFunctionURLRequest{Headers: headers, Body: `{"term": 3}`}, "gcp/compute")
	require.NoError(t, err)
}
