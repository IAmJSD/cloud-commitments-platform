package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Rows stored before the write side summarized provider errors hold raw text;
// the freshness response must show only a category message for them.
func TestFreshness_SanitizesStoredCollectionError(t *testing.T) {
	ctx := context.Background()
	rawStored := "azure: PUT https://management.azure.com/subscriptions/" + fakeSubID + "/providers/Microsoft.Capacity/x\n" +
		"RESPONSE 403: Forbidden\nERROR CODE: AuthorizationFailed\n; aws: operation error EC2: Op, https response error StatusCode: 403, " +
		"RequestID: " + fakeRequestID + ", api error UnauthorizedOperation: User: " + fakeRoleARN + " in account 123456789012"
	newCategory := "gcp: the provider returned an error"

	for name, tc := range map[string]struct{ stored, want string }{
		"raw text":      {rawStored, "the cloud provider refused the purchase: CUDly's role is not allowed to make it in this account"},
		"category form": {newCategory, newCategory},
	} {
		t.Run(name, func(t *testing.T) {
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			now := time.Now().UTC()
			stored := tc.stored
			store.On("GetRecommendationsFreshness", ctx).Return(&config.RecommendationsFreshness{LastCollectedAt: &now, LastCollectionError: &stored}, nil)
			h := &Handler{config: store, apiKey: "test-key"}

			got, err := h.getRecommendationsFreshness(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "test-key"}})

			require.NoError(t, err)
			require.NotNil(t, got.LastCollectionError)
			assertNoProviderDetail(t, "freshness", *got.LastCollectionError)
			assert.Equal(t, tc.want, *got.LastCollectionError)
			assert.Equal(t, tc.stored, stored, "the stored value is never rewritten")
		})
	}
}

// The refresh path re-reads freshness after the marked collection and hands it
// back to its caller; a stored raw provider error must come back as a category
// message on both the async-invoke and the synchronous branch.
func TestRunMarkedCollection_SanitizesStoredCollectionError(t *testing.T) {
	ctx := context.Background()
	raw := "aws: operation error EC2: Op, https response error StatusCode: 403, RequestID: " + fakeRequestID +
		", api error UnauthorizedOperation: User: " + fakeRoleARN + " in account 123456789012" +
		"; azure: GET https://management.azure.com/subscriptions/" + fakeSubID + "/providers/Microsoft.Capacity/x"
	want := "the cloud provider refused the purchase: CUDly's role is not allowed to make it in this account"

	for name, tc := range map[string]struct {
		lambdaARN string
		invoker   LambdaInvokerInterface
		sched     bool
	}{
		"async invoke": {lambdaARN: "arn:aws:lambda:us-east-1:123456789012:function:cudly", invoker: &stubLambdaInvoker{
			invokeFn: func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
				return &lambda.InvokeOutput{}, nil
			}}},
		"synchronous": {sched: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SCHEDULER_LAMBDA_ARN", tc.lambdaARN)
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			stored := raw
			store.On("GetRecommendationsFreshness", mock.Anything).Return(&config.RecommendationsFreshness{LastCollectionError: &stored}, nil)
			h := &Handler{config: store}
			if tc.invoker != nil {
				h.lambdaInvoker = tc.invoker
			}
			if tc.sched {
				sched := new(MockScheduler)
				t.Cleanup(func() { sched.AssertExpectations(t) })
				sched.On("CollectRecommendations", mock.Anything, "tok").Return(&scheduler.CollectResult{}, nil)
				h.scheduler = sched
			}

			got, err := h.runMarkedCollection(ctx, "tok")

			require.NoError(t, err)
			require.NotNil(t, got.LastCollectionError)
			assertNoProviderDetail(t, "refresh", *got.LastCollectionError)
			assert.Equal(t, want, *got.LastCollectionError)
		})
	}
}
