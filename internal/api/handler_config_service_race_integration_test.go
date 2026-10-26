//go:build integration

package api

// Real-Postgres race tests for the service-config PUT (issue #1488). The PUT
// reads the stored row, overlays only the keys the body carries and writes the
// whole row back. Unless that read-merge-write is one locked transaction, two
// admins saving different fields of the same row at the same moment both read
// the same base and the later write silently drops the earlier change. A mock
// cannot hold a row lock, so these run against a real database.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

// serviceConfigRaceRounds is how many concurrent PUT pairs each test fires.
// Before the fix one update was lost in nearly every round, so a handful
// would do; more rounds make a regression all but certain to show.
const serviceConfigRaceRounds = 40

type serviceConfigRaceFixture struct {
	pg      *testhelpers.PostgresContainer
	store   *config.PostgresStore
	handler *Handler
}

func newServiceConfigRaceFixture(ctx context.Context, t *testing.T) *serviceConfigRaceFixture {
	t.Helper()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store := config.NewPostgresStore(pg.DB)
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com",
	}, nil)
	mockAuth.grantAdmin()
	return &serviceConfigRaceFixture{pg: pg, store: store, handler: &Handler{config: store, auth: mockAuth}}
}

// putConcurrently fires every body as a PUT to aws/rds at the same moment and
// fails the test if any of them errors.
func (f *serviceConfigRaceFixture) putConcurrently(ctx context.Context, t *testing.T, bodies ...string) {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, len(bodies))
	var wg sync.WaitGroup
	for i, body := range bodies {
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			<-start
			_, errs[i] = f.handler.updateServiceConfig(ctx, &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"Authorization": "Bearer admin-token"},
				Body:    body,
			}, "aws/rds")
		}(i, body)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "PUT %d (%s)", i, bodies[i])
	}
}

// TestUpdateServiceConfig_ConcurrentPartialPutsKeepBothKeys is the issue
// #1488 scenario: two PUTs of different keys on an existing row, at once.
// Both changes must persist, along with the fields neither PUT touched.
func TestUpdateServiceConfig_ConcurrentPartialPutsKeepBothKeys(t *testing.T) {
	ctx := context.Background()
	f := newServiceConfigRaceFixture(ctx, t)

	lost := 0
	for round := 1; round <= serviceConfigRaceRounds; round++ {
		require.NoError(t, f.store.SaveServiceConfig(ctx, &config.ServiceConfig{
			Provider: "aws", Service: "rds", Enabled: true, Term: 3, Payment: "all-upfront",
			Coverage: 50, RampSchedule: config.RampImmediate, IncludeEngines: []string{"mysql"},
		}))
		coverage, minCount := float64(10+round), round

		f.putConcurrently(ctx, t,
			fmt.Sprintf(`{"coverage":%v}`, coverage),
			fmt.Sprintf(`{"min_count":%d}`, minCount))

		got, err := f.store.GetServiceConfig(ctx, "aws", "rds")
		require.NoError(t, err)
		if got.Coverage != coverage || got.MinCount != minCount {
			lost++
			t.Logf("round %d: coverage=%v (want %v) min_count=%d (want %d)", round, got.Coverage, coverage, got.MinCount, minCount)
		}
		require.Equal(t, []any{true, 3, "all-upfront", []string{"mysql"}},
			[]any{got.Enabled, got.Term, got.Payment, got.IncludeEngines}, "round %d: untouched fields must survive", round)
	}
	require.Zero(t, lost, "%d of %d rounds lost one of two concurrent partial PUTs", lost, serviceConfigRaceRounds)
}

// TestUpdateServiceConfig_ConcurrentFirstSavesKeepBothKeys covers the same race
// when no row exists yet: a row lock cannot serialize two saves that both find
// nothing to lock, so each would insert its own full row and the second would
// overwrite the first. Both changes must persist.
func TestUpdateServiceConfig_ConcurrentFirstSavesKeepBothKeys(t *testing.T) {
	ctx := context.Background()
	f := newServiceConfigRaceFixture(ctx, t)

	lost := 0
	for round := 1; round <= serviceConfigRaceRounds; round++ {
		_, err := f.pg.DB.Pool().Exec(ctx, `DELETE FROM service_configs WHERE provider = 'aws' AND service = 'rds'`)
		require.NoError(t, err)
		coverage, minCount := float64(10+round), round

		f.putConcurrently(ctx, t,
			fmt.Sprintf(`{"term":1,"payment":"no-upfront","coverage":%v}`, coverage),
			fmt.Sprintf(`{"term":1,"payment":"no-upfront","min_count":%d}`, minCount))

		got, err := f.store.GetServiceConfig(ctx, "aws", "rds")
		require.NoError(t, err)
		if got.Coverage != coverage || got.MinCount != minCount {
			lost++
			t.Logf("round %d: coverage=%v (want %v) min_count=%d (want %d)", round, got.Coverage, coverage, got.MinCount, minCount)
		}
	}
	require.Zero(t, lost, "%d of %d rounds lost one of two concurrent first saves", lost, serviceConfigRaceRounds)
}
