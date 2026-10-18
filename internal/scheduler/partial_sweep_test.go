package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hostExternalID is the synthetic AWS host identity the self-account
// collector verifies against STS.
const hostExternalID = "111111111111"

func azResponseError(statusCode int, code string) *azcore.ResponseError {
	return &azcore.ResponseError{
		StatusCode: statusCode,
		ErrorCode:  code,
		RawResponse: &http.Response{
			StatusCode: statusCode,
			Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "management.azure.com"}},
		},
	}
}

func TestClassifySweepCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, config.PartialReasonOther},
		{"plain error", errors.New("something odd for subscription 0000"), config.PartialReasonOther},
		{"canceled", fmt.Errorf("sub-1: %w", context.Canceled), config.PartialReasonCanceled},
		{"deadline", fmt.Errorf("sub-1: %w", context.DeadlineExceeded), config.PartialReasonTimeout},
		{"azure 403", fmt.Errorf("query: %w", azResponseError(403, "")), config.PartialReasonPermissionDenied},
		{"azure AuthorizationFailed code", azResponseError(400, "AuthorizationFailed"), config.PartialReasonPermissionDenied},
		{"azure 429", azResponseError(429, ""), config.PartialReasonThrottled},
		{"azure 404", azResponseError(404, "SubscriptionNotFound"), config.PartialReasonNotFound},
		{"azure 503", azResponseError(503, ""), config.PartialReasonProviderUnavailable},
		{"azure 400 unknown code", azResponseError(400, "BadRequest"), config.PartialReasonOther},
		{"gcp REST 403", &googleapi.Error{Code: 403}, config.PartialReasonPermissionDenied},
		{"gcp REST 429", fmt.Errorf("x: %w", &googleapi.Error{Code: 429}), config.PartialReasonThrottled},
		{"gcp gRPC permission", status.Error(codes.PermissionDenied, "denied"), config.PartialReasonPermissionDenied},
		{"gcp gRPC exhausted", status.Error(codes.ResourceExhausted, "quota"), config.PartialReasonThrottled},
		{"gcp gRPC unavailable", status.Error(codes.Unavailable, "down"), config.PartialReasonProviderUnavailable},
		{"aws throttling code", fmt.Errorf("op: %w", &smithy.GenericAPIError{Code: "ThrottlingException"}), config.PartialReasonThrottled},
		{"aws access denied code", &smithy.GenericAPIError{Code: "AccessDeniedException"}, config.PartialReasonPermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifySweepCause(tc.err))
		})
	}
}

func TestMergeSweepGaps(t *testing.T) {
	assert.Nil(t, mergeSweepGaps(nil, nil), "two complete sweeps stay complete")
	assert.Nil(t, (*sweepGap)(nil).forAccount(), "a complete sweep is not an incomplete account")

	a := &sweepGap{attemptedScopes: 3, failedScopes: 1, reasons: map[string]int{config.PartialReasonThrottled: 1}}
	b := &sweepGap{failedScopes: 2, failedDetails: 4, reasons: map[string]int{config.PartialReasonThrottled: 1, config.PartialReasonOther: 1}}

	got := mergeSweepGaps(a, b)
	assert.Equal(t, &sweepGap{
		attemptedScopes: 3, failedScopes: 3, failedDetails: 4,
		reasons: map[string]int{config.PartialReasonThrottled: 2, config.PartialReasonOther: 1},
	}, got)
	assert.Equal(t, 1, a.failedScopes, "merge must not mutate its inputs")
	assert.Equal(t, 1, a.reasons[config.PartialReasonThrottled], "merge must not mutate its inputs")

	acct := got.forAccount()
	assert.Equal(t, 1, acct.incompleteAccounts)
	assert.Zero(t, got.incompleteAccounts, "forAccount must not mutate the scope-level gap")

	two := mergeSweepGaps(acct, b.forAccount())
	assert.Equal(t, 2, two.incompleteAccounts)
}

// The persisted summary is served to every view:recommendations user, so it
// must not carry the failed subscription IDs or raw provider error text.
func TestSweepGap_ProviderSweepIsSanitized(t *testing.T) {
	partial := &azureprovider.PartialSubscriptionFailureError{
		Attempted: 4,
		Succeeded: 2,
		Failed: []azureprovider.SubscriptionFailure{
			{SubscriptionID: "sub-secret-a", Err: fmt.Errorf("token for tenant tenant-xyz rejected: %w", azResponseError(403, ""))},
			{SubscriptionID: "sub-secret-b", Err: errors.New("raw provider text arn:aws:iam::role/x")},
		},
	}
	sweep := gapFromPartialSubscriptions(partial).forAccount().toProviderSweep("azure")

	assert.Equal(t, config.PartialProviderSweep{
		Provider:           "azure",
		IncompleteAccounts: 1,
		AttemptedScopes:    4,
		FailedScopes:       2,
		Reasons:            map[string]int{config.PartialReasonPermissionDenied: 1, config.PartialReasonOther: 1},
	}, sweep)

	encoded, err := json.Marshal(sweep)
	require.NoError(t, err)
	for _, leak := range []string{"sub-secret-a", "sub-secret-b", "tenant-xyz", "arn:aws", "raw provider text"} {
		assert.NotContains(t, string(encoded), leak)
	}
}

// TestCollectProvider_RegisteredAccountPartialSweep drives the registered-
// account path (the common production shape) for the providers whose
// per-account collector can be wired without real credentials: AWS
// self-account and GCP application-default. Hardcoding a nil gap in
// collectAWSForAccount or collectGCPForAccount fails this test (A16-007).
func TestCollectProvider_RegisteredAccountPartialSweep(t *testing.T) {
	for _, tc := range []struct {
		provider   string
		account    config.CloudAccount
		partialErr error
		collect    func(*Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error)
	}{
		{
			provider: "aws",
			account: config.CloudAccount{ID: "acct-aws", Name: "aws", Provider: "aws", AWSAuthMode: "role_arn",
				ExternalID: hostExternalID, Enabled: true},
			partialErr: partialSweepCases()[0].partialErr,
			collect: func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
				return s.collectAWSRecommendations(context.Background(), &config.GlobalConfig{})
			},
		},
		{
			provider: "gcp",
			account: config.CloudAccount{ID: "acct-gcp", Name: "gcp", Provider: "gcp", GCPAuthMode: "application_default",
				GCPProjectID: "proj-a", ExternalID: "proj-a", Enabled: true},
			partialErr: partialSweepErr("proj-b", "proj-c"),
			collect: func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
				return s.collectGCPRecommendations(context.Background(), &config.GlobalConfig{})
			},
		},
	} {
		for _, partial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/partial=%v", tc.provider, partial), func(t *testing.T) {
				var sweepErr error
				if partial {
					sweepErr = tc.partialErr
				}
				s, store := newPartialSweepScheduler(t, tc.provider, sweepErr)
				s.stsClient = &fakeSTSClient{accountID: hostExternalID}
				store.ExpectedCalls = nil // replace the ambient-path listing with one registered account
				store.On("ListCloudAccounts", mock.Anything, mock.Anything).
					Return([]config.CloudAccount{tc.account}, nil).Once()

				recs, succeeded, gap, err := tc.collect(s)

				require.NoError(t, err)
				assert.NotEmpty(t, recs, "the scopes that answered must still be upserted")
				if !partial {
					assert.Equal(t, []string{tc.account.ID}, succeeded)
					assert.Nil(t, gap)
					return
				}
				assert.Empty(t, succeeded, "a partially-swept account must not authorize eviction")
				require.NotNil(t, gap)
				assert.Equal(t, 1, gap.incompleteAccounts)
				assert.Equal(t, 2, gap.failedScopes)
			})
		}
	}
}

// expectScheduledCollection wires the store calls CollectRecommendations
// makes on the scheduled (cron, tokenless) path and returns pointers that
// capture what was persisted.
func expectScheduledCollection(store *MockConfigStore, providers ...string) (*[]config.RecommendationRecord, **config.RecommendationsCollectionPartial) {
	store.On("GetGlobalConfig", mock.Anything).
		Return(&config.GlobalConfig{EnabledProviders: providers}, nil).Once()

	var upserted []config.RecommendationRecord
	store.On("UpsertRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			upserted = args.Get(2).([]config.RecommendationRecord)
		}).Return(nil).Once()

	// Registered as Maybe so an unwanted call is RECORDED: MockConfigStore
	// short-circuits unexpected methods before mock.Called, which would
	// make AssertNotCalled pass vacuously.
	var recorded *config.RecommendationsCollectionPartial
	store.On("SetRecommendationsCollectionPartial", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			recorded = args.Get(1).(*config.RecommendationsCollectionPartial)
		}).Return(nil).Maybe()
	return &upserted, &recorded
}

// TestCollectRecommendations_PartialSweepRecordedInState is the scheduled-
// path regression for #87: a sweep where a strict subset of subscriptions
// fails must persist BOTH the surviving recommendations and the
// partial-sweep marker the freshness API serves.
func TestCollectRecommendations_PartialSweepRecordedInState(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-a")
	s, store := newPartialSweepScheduler(t, "azure", partialSweepErr("sub-b", "sub-c"))
	upserted, recorded := expectScheduledCollection(store, "azure")

	result, err := s.CollectRecommendations(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, []string{"azure"}, result.SuccessfulProviders, "a partial sweep is not a failed provider")
	assert.Len(t, *upserted, 1, "the surviving subscription's recommendation must still be stored")
	require.NotNil(t, *recorded, "the partial sweep must be recorded in recommendations_state")
	assert.Equal(t, &config.RecommendationsCollectionPartial{
		Providers: []config.PartialProviderSweep{{
			Provider:           "azure",
			IncompleteAccounts: 1,
			AttemptedScopes:    3,
			FailedScopes:       2,
			Reasons:            map[string]int{config.PartialReasonOther: 2},
		}},
	}, *recorded)
	store.AssertNotCalled(t, "SetRecommendationsCollectionError", mock.Anything, mock.Anything)
}

// TestCollectRecommendations_CompleteSweepLeavesPartialCleared: a complete
// sweep records no partial marker. UpsertRecommendations resets the column
// (see the store integration test), so the previous run's marker clears.
func TestCollectRecommendations_CompleteSweepLeavesPartialCleared(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-a")
	s, store := newPartialSweepScheduler(t, "azure", nil)
	upserted, _ := expectScheduledCollection(store, "azure")

	_, err := s.CollectRecommendations(context.Background(), "")

	require.NoError(t, err)
	assert.Len(t, *upserted, 1)
	store.AssertNotCalled(t, "SetRecommendationsCollectionPartial", mock.Anything, mock.Anything)
}

// TestCollectRecommendations_TotalFailureUnchanged: a hard provider failure
// keeps the pre-#87 behavior -- the provider is failed and named in
// last_collection_error -- and is never reported as a partial sweep.
func TestCollectRecommendations_TotalFailureUnchanged(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-a")
	s, store := newPartialSweepScheduler(t, "azure", errors.New("credentials expired"))
	expectScheduledCollection(store, "azure")
	var recordedErr string
	store.On("SetRecommendationsCollectionError", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { recordedErr = args.String(1) }).Return(nil).Once()

	result, err := s.CollectRecommendations(context.Background(), "")

	require.NoError(t, err)
	assert.Contains(t, result.FailedProviders, "azure")
	assert.Contains(t, recordedErr, "azure: ")
	store.AssertNotCalled(t, "SetRecommendationsCollectionPartial", mock.Anything, mock.Anything)
}

// A partial provider and a complete one: only the partial provider appears.
func TestAppendPartialProvider(t *testing.T) {
	assert.Nil(t, appendPartialProvider(nil, "aws", nil))
	got := appendPartialProvider(nil, "azure", &sweepGap{failedScopes: 1, reasons: map[string]int{}})
	got = appendPartialProvider(got, "gcp", nil)
	require.Len(t, got.Providers, 1)
	assert.Equal(t, "azure", got.Providers[0].Provider)
	assert.NotNil(t, got.Providers[0].Reasons, "reasons must encode as {} rather than null")
}
