//go:build integration

package scheduler

import (
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuppressionsScopedToPurchasedVariant_RealPostgres is the issue #816
// regression on a real database: recs are persisted and listed through the
// real store, the purchase's suppressions are built by BuildSuppressions and
// written by CreateSuppressionTx, and ListRecommendations subtracts them.
// Buying us-east-1a Linux/default must not hide the us-east-1b, regional,
// Windows or dedicated siblings. A row written the pre-#816 way (no variant
// columns, as old code inserts during a rolling deploy) still applies to every
// variant of its 6-tuple.
func TestSuppressionsScopedToPurchasedVariant_RealPostgres(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)

	rec := func(id, resourceType, az, platform, tenancy string) config.RecommendationRecord {
		return config.RecommendationRecord{
			ID: id, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: resourceType,
			Count: 5, Term: 1, Payment: "all-upfront", AvailabilityZone: az,
			Details: []byte(`{"availability_zone":"` + az + `","platform":"` + platform + `","tenancy":"` + tenancy + `"}`),
		}
	}
	bought := rec("bought", "m5.large", "us-east-1a", "Linux/UNIX", "default")
	records := []config.RecommendationRecord{
		bought,
		rec("other-az", "m5.large", "us-east-1b", "Linux/UNIX", "default"),
		rec("regional", "m5.large", "", "Linux/UNIX", "default"),
		rec("windows", "m5.large", "us-east-1a", "Windows", "default"),
		rec("dedicated", "m5.large", "us-east-1a", "Linux/UNIX", "dedicated"),
		rec("legacy-a", "c5.large", "us-east-1a", "Linux/UNIX", "default"),
		rec("legacy-b", "c5.large", "us-east-1b", "Windows", "default"),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now(), records, nil))

	purchase := bought
	purchase.Count = 3
	sups := config.BuildSuppressions([]config.RecommendationRecord{purchase}, uuid.NewString(), &config.GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error { return store.CreateSuppressionTx(ctx, tx, &sups[0]) }))

	// The insert old code issues: 6-tuple only, variant columns left NULL.
	_, err := pg.DB.Pool().Exec(ctx, `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at)
		VALUES ($1, '', 'aws', 'ec2', 'us-east-1', 'c5.large', '', 1, NOW() + INTERVAL '1 day')`, uuid.NewString())
	require.NoError(t, err)

	s := &Scheduler{config: store, isLambda: true}
	got, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	counts := map[string]int{}
	for _, r := range got {
		counts[r.ID] = r.Count
	}
	assert.Equal(t, map[string]int{
		"bought": 2, "other-az": 5, "regional": 5, "windows": 5, "dedicated": 5,
		"legacy-a": 4, "legacy-b": 4,
	}, counts)
}

// TestLegacyEC2RetrySuppressesBoughtVariant_RealPostgres is the issue #1232
// regression: a retried execution persisted before Details existed carries an
// EC2 rec with no platform or tenancy. Its suppression must wildcard the
// 6-tuple, so the later Linux/UNIX recommendation that was actually bought is
// filtered instead of re-offered.
func TestLegacyEC2RetrySuppressesBoughtVariant_RealPostgres(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)

	later := config.RecommendationRecord{
		ID: "later", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Count: 3, Term: 1, Payment: "all-upfront", AvailabilityZone: "us-east-1a",
		Details: []byte(`{"availability_zone":"us-east-1a","platform":"Linux/UNIX","tenancy":"default"}`),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now(), []config.RecommendationRecord{later}, nil))

	legacy := later
	legacy.ID = "legacy"
	legacy.Count = 3
	legacy.Details = nil
	sups := config.BuildSuppressions([]config.RecommendationRecord{legacy}, uuid.NewString(), &config.GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error { return store.CreateSuppressionTx(ctx, tx, &sups[0]) }))

	s := &Scheduler{config: store, isLambda: true}
	got, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	assert.Empty(t, got, "the bought variant must be fully suppressed")
}
