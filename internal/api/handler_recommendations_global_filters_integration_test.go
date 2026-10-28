//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type globalFilterProvider struct {
	provider.FactoryInterface
	provider.Provider
	provider.RecommendationsClient
}

func (p globalFilterProvider) CreateAndValidateProvider(_ context.Context, name string, cfg *provider.Config) (provider.Provider, error) {
	if name != "aws" || cfg != nil {
		return nil, fmt.Errorf("unexpected provider request: %s", name)
	}
	return p, nil
}

func (p globalFilterProvider) GetRecommendationsClient(context.Context) (provider.RecommendationsClient, error) {
	return p, nil
}

func (globalFilterProvider) GetAllRecommendations(context.Context) ([]common.Recommendation, error) {
	return []common.Recommendation{{Provider: common.ProviderAWS, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront", EstimatedSavings: 100}}, nil
}

type globalFilterSTS struct{}

func (globalFilterSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}, nil
}

func TestGlobalFiltersCollectedRecommendationsAPI(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("registered=%t", registered), func(t *testing.T) {
			ctx := t.Context()
			pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
			store := config.NewPostgresStore(pg.DB)
			require.NoError(t, store.SaveGlobalConfig(ctx, &config.GlobalConfig{EnabledProviders: []string{"aws"}, DefaultTerm: 1, DefaultPayment: "all-upfront"}))
			account := uuid.NewString()
			if registered {
				require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{ID: account, Name: "host", Provider: "aws", ExternalID: "123456789012", Enabled: false}))
			}
			collector := scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, ProviderFactory: globalFilterProvider{}, STSClient: globalFilterSTS{}, EmailSender: &stubEmailNotifier{}, IsLambda: true})
			result, err := collector.CollectRecommendations(ctx, "")
			require.NoError(t, err)
			require.Empty(t, result.FailedProviders)
			require.Equal(t, 1, result.Recommendations)
			rows, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			if registered {
				require.Equal(t, &account, rows[0].CloudAccountID)
			} else {
				require.Nil(t, rows[0].CloudAccountID)
			}
			var nulls int
			require.NoError(t, pg.DB.Pool().QueryRow(ctx, "SELECT count(*) FROM recommendations WHERE cloud_account_id IS NULL").Scan(&nulls))
			assert.Equal(t, !registered, nulls == 1)
			authStore := auth.NewPostgresStore(pg.DB)
			csrf := []byte(strings.Repeat("f", 32))
			_, token, _ := marketplaceSession(t, authStore, []auth.Permission{{Action: auth.ActionView, Resource: "recommendations"}}, []string{"*"}, csrf)
			h := NewHandler(HandlerConfig{ConfigStore: store, Scheduler: collector, AuthService: &marketplaceAuthFixture{service: auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrf})}})
			request := func(path string, out any) {
				t.Helper()
				response, err := h.HandleRequest(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer " + token}, RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: path}}})
				require.NoError(t, err)
				require.Equal(t, 200, response.StatusCode, response.Body)
				require.NoError(t, json.Unmarshal([]byte(response.Body), out))
			}
			var initial RecommendationsResponse
			request("/api/recommendations", &initial)
			require.Len(t, initial.Recommendations, 1, "no global configuration")
			for _, enabled := range []bool{true, false, true} {
				require.NoError(t, store.SaveServiceConfig(ctx, &config.ServiceConfig{Provider: "aws", Service: "ec2", Enabled: enabled, Term: 1, Payment: "all-upfront", Coverage: 100}))
				var list RecommendationsResponse
				request("/api/recommendations", &list)
				var detail RecommendationDetailResponse
				request("/api/recommendations/"+rows[0].ID+"/detail", &detail)
				var summary DashboardSummaryResponse
				request("/api/dashboard/summary", &summary)
				if enabled {
					assert.Len(t, list.Recommendations, 1)
					assert.Equal(t, 1, summary.TotalRecommendations)
					assert.Equal(t, 100.0, summary.PotentialMonthlySavings)
					assert.Empty(t, detail.HiddenBy)
				} else {
					assert.Empty(t, list.Recommendations)
					assert.Zero(t, summary.TotalRecommendations)
					assert.Zero(t, summary.PotentialMonthlySavings)
					assert.Equal(t, []string{"enabled=false"}, detail.HiddenBy)
				}
			}
		})
	}
}
