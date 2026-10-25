//go:build integration

package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type identitySTS struct {
	credentials.STSClient
	keys map[string]string
}

func (s identitySTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	key, ok := s.keys[aws.ToString(in.RoleArn)]
	if !ok {
		return nil, fmt.Errorf("unexpected fixture role %q", aws.ToString(in.RoleArn))
	}
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String(key), SecretAccessKey: aws.String("fixture-only"),
		SessionToken: aws.String("fixture-only"), Expiration: aws.Time(time.Now().Add(time.Hour)),
	}}, nil
}

type identityProviderFactory struct {
	provider.FactoryInterface
	recs map[string][]common.Recommendation
}

func (f identityProviderFactory) CreateAndValidateProvider(ctx context.Context, name string, cfg *provider.ProviderConfig) (provider.Provider, error) {
	if name != "aws" || cfg == nil || cfg.AWSCredentialsProvider == nil {
		return nil, fmt.Errorf("expected registered AWS fixture credentials")
	}
	creds, err := cfg.AWSCredentialsProvider.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	recs, ok := f.recs[creds.AccessKeyID]
	if !ok {
		return nil, fmt.Errorf("unknown fixture credential")
	}
	return identityProvider{recs: recs}, nil
}

type identityProvider struct {
	provider.Provider
	recs []common.Recommendation
}

func (p identityProvider) GetRecommendationsClient(context.Context) (provider.RecommendationsClient, error) {
	return identityRecommendations{recs: p.recs}, nil
}

type identityRecommendations struct {
	provider.RecommendationsClient
	recs []common.Recommendation
}

func (c identityRecommendations) GetAllRecommendations(context.Context) ([]common.Recommendation, error) {
	return c.recs, nil
}

type identityEmail struct{ email.SenderInterface }

func (identityEmail) SendNewRecommendationsNotification(context.Context, email.NotificationData) error {
	return nil
}

func TestRecommendationIdentity_CollectionPostgresDetailAndPricing(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)
	require.NoError(t, store.SaveGlobalConfig(ctx, &config.GlobalConfig{
		EnabledProviders: []string{"aws"}, DefaultTerm: 1, DefaultPayment: "all-upfront",
	}))
	accounts := []config.CloudAccount{
		{Provider: "aws", Name: "Identity A", ExternalID: "111111111111", AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::111111111111:role/identity-fixture", Enabled: true},
		{Provider: "aws", Name: "Identity B", ExternalID: "222222222222", AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::222222222222:role/identity-fixture", Enabled: true},
	}
	stsFixture := identitySTS{keys: make(map[string]string)}
	factory := identityProviderFactory{recs: make(map[string][]common.Recommendation)}
	for i := range accounts {
		require.NoError(t, store.CreateCloudAccount(ctx, &accounts[i]))
		key := fmt.Sprintf("identity-fixture-%d", i)
		stsFixture.keys[accounts[i].AWSRoleARN] = key
		tenancy := []string{"default", "dedicated"}[i]
		base := common.Recommendation{
			Provider: common.ProviderAWS, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large",
			Count: 2*i + 1, Term: "1yr", PaymentOption: "all-upfront", CommitmentCost: float64(1000 * (i + 1)),
			EstimatedSavings: float64(100 * (i + 1)), UsageHistory: []float64{float64(50 + i)},
			Details: &common.ComputeDetails{Tenancy: tenancy, Platform: "Linux/UNIX", Scope: "Region"},
		}
		variant := base
		variant.Term, variant.PaymentOption, variant.CommitmentCost = "3yr", "partial-upfront", base.CommitmentCost*2
		mismatch := base
		mismatch.Term = "3yr"
		mismatch.Details = &common.ComputeDetails{Tenancy: "host", Platform: "Linux/UNIX", Scope: "Region"}
		factory.recs[key] = []common.Recommendation{base, variant, mismatch}
	}
	collector := scheduler.NewScheduler(scheduler.SchedulerConfig{
		ConfigStore: store, ProviderFactory: factory, AssumeRoleSTS: stsFixture, EmailSender: identityEmail{},
	})
	result, err := collector.CollectRecommendations(ctx, "")
	require.NoError(t, err)
	require.Empty(t, result.FailedProviders)
	require.Equal(t, 6, result.Recommendations)
	rows, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 6)
	ids := make(map[string]bool)
	for _, row := range rows {
		assert.False(t, ids[row.ID], "different persisted account/cell must have distinct ID: %s", row.ID)
		ids[row.ID] = true
	}
	for i, account := range accounts {
		t.Run(account.Name, func(t *testing.T) {
			own, listErr := store.ListStoredRecommendations(ctx, config.RecommendationFilter{AccountIDs: []string{account.ID}})
			require.NoError(t, listErr)
			require.Len(t, own, 3)
			var base, variant config.RecommendationRecord
			for _, row := range own {
				if row.Term == 1 {
					base = row
				}
				if row.Payment == "partial-upfront" {
					variant = row
				}
			}
			require.NotEmpty(t, base.ID)
			require.NotEmpty(t, variant.ID)
			authFixture := new(MockAuthService)
			authFixture.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil)
			authFixture.grantScoped(account.ID)
			h := &Handler{config: store, scheduler: collector, auth: authFixture}
			req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer " + scopedToken}}
			for _, row := range own {
				got, _, lookupErr := collector.GetRecommendationByID(ctx, row.ID)
				require.NoError(t, lookupErr)
				require.NotNil(t, got)
				assert.Equal(t, account.ID, *got.CloudAccountID)
				assert.Equal(t, row.UpfrontCost, got.UpfrontCost)
				assert.Equal(t, 2*i+1, got.Count)
				assert.Equal(t, []float64{float64(50 + i)}, got.UsageHistory)
				assert.JSONEq(t, string(row.Details), string(got.Details))
				detail, detailErr := h.getRecommendationDetail(ctx, req, row.ID)
				if assert.NoError(t, detailErr) {
					assert.Equal(t, row.ID, detail.ID)
					assert.Equal(t, []string{"medium", "high"}[i], detail.ConfidenceBucket)
				}
			}
			for _, other := range rows {
				if *other.CloudAccountID != account.ID {
					_, err = h.getRecommendationDetail(ctx, req, other.ID)
					assert.ErrorIs(t, err, errNotFound)
				}
			}
			for _, target := range []config.RecommendationRecord{base, variant} {
				request := target
				request.ID, request.UpfrontCost = base.ID, 1 // Details stay: the modal echoes them and platform/tenancy are part of the identity key
				priced := []config.RecommendationRecord{request}
				if assert.NoError(t, h.priceRecommendationsFromStore(ctx, priced)) {
					assert.Equal(t, account.ID, *priced[0].CloudAccountID)
					assert.Equal(t, target.UpfrontCost, priced[0].UpfrontCost)
					assert.Equal(t, target.Count, priced[0].Count)
					assert.JSONEq(t, string(target.Details), string(priced[0].Details))
				}
			}
			mismatch := base
			mismatch.Term = 3
			err = h.priceRecommendationsFromStore(ctx, []config.RecommendationRecord{mismatch})
			assert.ErrorContains(t, err, "not in the current recommendation set")
			stale := base
			stale.ID = "stale-before-refresh"
			err = h.priceRecommendationsFromStore(ctx, []config.RecommendationRecord{stale})
			clientErr, ok := IsClientError(err)
			require.True(t, ok)
			assert.Equal(t, 409, clientErr.code)
			authFixture.AssertExpectations(t)
		})
	}
	result, err = collector.CollectRecommendations(ctx, "")
	require.NoError(t, err)
	require.Empty(t, result.FailedProviders)
	require.Equal(t, 6, result.Recommendations)
	refreshed, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, refreshed, len(rows))
	for _, row := range refreshed {
		assert.True(t, ids[row.ID], "ID changed on recollection")
	}
}
