//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// Issue #624: a scoped user asking analytics for the raw external id of their
// one registered account got a provider-unconstrained filter, so rows of
// ANOTHER provider's unregistered account with the same id came back too (an
// AWS account number and a GCP project number can coincide). The fixture
// registers aws/sharedID only and seeds, in both purchase_history and
// savings_snapshots, one row per provider for that id:
//
//   - aws:  the registered account's row, which the scoped user may see
//   - gcp:  an unregistered account sharing the id, which must stay hidden
//   - "":   a legacy row with no provider, which cannot be attributed to the
//     registered account and is hidden from scoped users too
//
// Each row has its own service so the response names exactly which rows were
// aggregated.
const (
	scopeSharedID   = "424242" // aws account number == gcp project number
	scopeSvcAWS     = "registered-aws"
	scopeSvcGCP     = "unregistered-gcp"
	scopeSvcLegacy  = "legacy-no-provider"
	scopeAWSAccount = "dev"
)

type analyticsScopeDB struct {
	pg      *testhelpers.PostgresContainer
	handler *Handler
	auth    *auth.PostgresStore
	awsUUID string
}

func setupAnalyticsScopeDB(t *testing.T) *analyticsScopeDB {
	t.Helper()
	ctx := t.Context()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))

	store := config.NewPostgresStore(pg.DB)
	aws := &config.CloudAccount{Name: scopeAWSAccount, Provider: "aws", ExternalID: scopeSharedID, Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, aws))

	snapshots := analytics.NewPostgresAnalyticsStore(pg.DB)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i, r := range []struct{ provider, service string }{
		{"aws", scopeSvcAWS},
		{"gcp", scopeSvcGCP},
		{"", scopeSvcLegacy},
	} {
		_, err := pg.DB.Pool().Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region,
				resource_type, count, term, payment, upfront_cost, monthly_cost, estimated_savings)
			VALUES ($1, $2, $3, $4, $5, 'us-east-1', 'm5.large', 1, 1, 'all-upfront', 10, 0, $6)`,
			scopeSharedID, fmt.Sprintf("purchase-%d", i), ts, r.provider, r.service, float64(i+1))
		require.NoError(t, err)
		require.NoError(t, snapshots.SaveSnapshot(ctx, &analytics.SavingsSnapshot{
			AccountID: scopeSharedID, Timestamp: ts, Provider: r.provider, Service: r.service,
			Region: "us-east-1", CommitmentType: "RI", TotalSavings: float64(i + 1),
		}))
	}
	require.NoError(t, snapshots.RefreshMaterializedViews(ctx))

	authStore := auth.NewPostgresStore(pg.DB)
	csrf := []byte(strings.Repeat("a", 32))
	h := NewHandler(HandlerConfig{
		ConfigStore:        store,
		AnalyticsClient:    NewPostgresAnalyticsClient(pg.DB),
		AnalyticsSnapshots: snapshots,
		AuthService:        &marketplaceAuthFixture{service: auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrf})},
	})
	return &analyticsScopeDB{pg: pg, handler: h, auth: authStore, awsUUID: aws.ID}
}

func (db *analyticsScopeDB) session(t *testing.T, allowed []string) string {
	t.Helper()
	_, token, _ := marketplaceSession(t, db.auth, []auth.Permission{{Action: auth.ActionView, Resource: "purchases"}}, allowed, []byte(strings.Repeat("a", 32)))
	return token
}

// get issues one GET through HandleRequest and decodes a 200 body into out.
func (db *analyticsScopeDB) get(ctx context.Context, t *testing.T, token, path string, query map[string]string, out any) int {
	t.Helper()
	resp, err := db.handler.HandleRequest(ctx, &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer " + token},
		QueryStringParameters: query,
		RawQueryString:        encodeQuery(query),
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: path},
		},
	})
	require.NoError(t, err)
	if resp.StatusCode == 200 && out != nil {
		require.NoError(t, json.Unmarshal([]byte(resp.Body), out), resp.Body)
	}
	return resp.StatusCode
}

func encodeQuery(q map[string]string) string {
	v := url.Values{}
	for k, s := range q {
		v.Set(k, s)
	}
	return v.Encode()
}

// analyticsSeen is what one account_id request exposed across the endpoints.
type analyticsSeen struct {
	status                int
	historyProviders      []string // /api/history/analytics by_provider keys
	breakdownProviders    []string // /api/history/breakdown?dimension=provider keys
	trendsServices        []string // /api/analytics/trends by_provider services
	trendsMonthlyServices []string // /api/analytics/trends monthly services
	trendsByService       []string // /api/analytics/trends by_service services
}

// seen drives the three analytics endpoints for accountID and returns the
// status plus the sorted providers (purchase_history endpoints) or fixture
// services (savings-snapshot trends) each one aggregated.
func (db *analyticsScopeDB) seen(ctx context.Context, t *testing.T, token, accountID string) analyticsSeen {
	t.Helper()
	q := map[string]string{"account_id": accountID}
	var s analyticsSeen

	var hist AnalyticsResponse
	s.status = db.get(ctx, t, token, "/api/history/analytics", q, &hist)
	for _, dp := range hist.DataPoints {
		for p := range dp.ByProvider {
			s.historyProviders = append(s.historyProviders, p)
		}
	}

	var bd BreakdownResponse
	bq := map[string]string{"account_id": accountID, "dimension": "provider"}
	require.Equal(t, s.status, db.get(ctx, t, token, "/api/history/breakdown", bq, &bd))
	for p := range bd.Data {
		s.breakdownProviders = append(s.breakdownProviders, p)
	}

	var tr TrendsResponse
	require.Equal(t, s.status, db.get(ctx, t, token, "/api/analytics/trends", q, &tr))
	for _, b := range tr.Provider {
		s.trendsServices = append(s.trendsServices, b.Service)
	}
	for _, m := range tr.Monthly {
		s.trendsMonthlyServices = append(s.trendsMonthlyServices, m.Service)
	}
	for _, b := range tr.Service {
		s.trendsByService = append(s.trendsByService, b.Service)
	}
	for _, l := range [][]string{s.historyProviders, s.breakdownProviders, s.trendsServices, s.trendsMonthlyServices, s.trendsByService} {
		sort.Strings(l)
	}
	return s
}

func assertSeen(t *testing.T, got analyticsSeen, providers, services []string) {
	t.Helper()
	require.Equal(t, 200, got.status)
	assert.Equal(t, providers, got.historyProviders, "history/analytics")
	assert.Equal(t, providers, got.breakdownProviders, "history/breakdown")
	assert.Equal(t, services, got.trendsServices, "analytics/trends by_provider")
	assert.Equal(t, services, got.trendsMonthlyServices, "analytics/trends monthly")
	assert.Equal(t, services, got.trendsByService, "analytics/trends by_service")
}

func TestAnalyticsScopedRawExternalIDIsProviderScoped_DB(t *testing.T) {
	db := setupAnalyticsScopeDB(t)
	ctx := t.Context()

	// A scoped user reaches the registered account by its account ID or by its
	// raw external id; either way only the aws row may be aggregated. Scope
	// entries match account IDs only, never display names (#1034).
	for _, scope := range []string{db.awsUUID, scopeSharedID} {
		t.Run("scope "+scope+", raw id request", func(t *testing.T) {
			got := db.seen(ctx, t, db.session(t, []string{scope}), scopeSharedID)
			assertSeen(t, got, []string{"aws"}, []string{scopeSvcAWS})
		})
	}

	t.Run("scoped UUID request is unchanged", func(t *testing.T) {
		got := db.seen(ctx, t, db.session(t, []string{db.awsUUID}), db.awsUUID)
		assertSeen(t, got, []string{"aws"}, []string{scopeSvcAWS})
	})

	t.Run("unrestricted raw id request is unchanged", func(t *testing.T) {
		got := db.seen(ctx, t, db.session(t, []string{"*"}), scopeSharedID)
		all := []string{scopeSvcGCP, scopeSvcLegacy, scopeSvcAWS}
		sort.Strings(all)
		assertSeen(t, got, []string{"", "aws", "gcp"}, all)
	})
}

// A GCP account sharing the id can be registered (and removed) while scoped
// analytics requests run. Each request must decide on one account-list
// snapshot: either the id is unique and the response holds only the aws rows,
// or it is ambiguous and the request is refused. It must never aggregate the
// gcp or legacy rows.
func TestAnalyticsScopedRawExternalIDConcurrentRegistration_DB(t *testing.T) {
	db := setupAnalyticsScopeDB(t)
	ctx := t.Context()
	token := db.session(t, []string{db.awsUUID})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var id string
			if err := db.pg.DB.Pool().QueryRow(ctx,
				`INSERT INTO cloud_accounts (name, provider, external_id) VALUES ('gcp-twin', 'gcp', $1) RETURNING id::text`,
				scopeSharedID).Scan(&id); err != nil {
				t.Errorf("register gcp twin: %v", err)
				return
			}
			if _, err := db.pg.DB.Pool().Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, id); err != nil {
				t.Errorf("remove gcp twin: %v", err)
				return
			}
		}
	}()

	var ok, refused int
	for i := 0; i < 40; i++ {
		var hist AnalyticsResponse
		status := db.get(ctx, t, token, "/api/history/analytics", map[string]string{"account_id": scopeSharedID}, &hist)
		switch status {
		case 200:
			ok++
			for _, dp := range hist.DataPoints {
				for p := range dp.ByProvider {
					assert.Equal(t, "aws", p, "request %d aggregated a %q row", i, p)
				}
			}
		case 404:
			refused++
		default:
			t.Fatalf("request %d: unexpected status %d", i, status)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d answered with aws rows only, %d refused as ambiguous", ok, refused)
}
