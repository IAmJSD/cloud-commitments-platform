package purchase

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// lineageRig drives successive attempts of ONE execution lineage (the first
// run and its Retry successors) through executePurchase against ONE store, so
// whatever the store persisted on the first attempt is visible to the next.
// The plan's account set is mutable between attempts, which is the
// configuration change issue #97 is about.
type lineageRig struct {
	t            *testing.T
	store        *MockConfigStore
	manager      *Manager
	mu           sync.Mutex
	planAccounts []config.CloudAccount
	tokens       []string
}

// shapeTestAccountA is a cloud account that exists in config throughout; only
// its membership in the plan changes between attempts.
var (
	shapeTestAccountA = config.CloudAccount{
		ID: "acct-A", Name: "A", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys",
	}
	shapeTestAccountB = config.CloudAccount{
		ID: "acct-B", Name: "B", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "access_keys",
	}
)

func newLineageRig(t *testing.T) *lineageRig {
	t.Helper()
	r := &lineageRig{t: t, store: new(MockConfigStore)}
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)

	r.store.GetPurchasePlanFn = func(_ context.Context, id string) (*config.PurchasePlan, error) {
		return &config.PurchasePlan{ID: id, Name: "Plan X"}, nil
	}
	r.store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]config.CloudAccount(nil), r.planAccounts...), nil
	}
	r.store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		for _, acct := range []config.CloudAccount{shapeTestAccountA, shapeTestAccountB} {
			if acct.ID == id {
				return &acct, nil
			}
		}
		return nil, nil
	}
	r.store.SavePurchaseExecutionFn = func(_ context.Context, _ *config.PurchaseExecution) error { return nil }
	r.store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil).Maybe()
	r.store.On("CompletePlanStep", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil).Maybe()
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil).Maybe()
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(mockServiceClient, nil).Maybe()
	// The whole provider chain WOULD sell, so a guard that fails open shows up
	// as an extra or mismatched token rather than as a mock panic.
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.AnythingOfType("common.PurchaseOptions")).
		Run(func(args mock.Arguments) {
			r.mu.Lock()
			r.tokens = append(r.tokens, args.Get(2).(common.PurchaseOptions).IdempotencyToken)
			r.mu.Unlock()
		}).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil).Maybe()

	r.manager = &Manager{
		config:          r.store,
		email:           mockEmail,
		providerFactory: mockFactory,
		credStore:       awsAccessKeyCredStore(),
		dashboardURL:    "https://dashboard.example.com",
	}
	return r
}

func (r *lineageRig) setPlanAccounts(accounts ...config.CloudAccount) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planAccounts = accounts
}

// attempt runs one execution of the lineage and returns the provider tokens it
// purchased under (one entry == one real purchase) and executePurchase's error.
// Each attempt is a fresh row, as a Retry successor is: a new ExecutionID, the
// same IdempotencyKey, no cloud_account_id and unpurchased recommendations.
func (r *lineageRig) attempt(executionID string) ([]string, error) {
	r.t.Helper()
	return r.attemptKeyed(executionID, "lineage-97", scopedTestRec("acct-A"))
}

// attemptKeyed is attempt with an explicit lineage key and recommendations.
func (r *lineageRig) attemptKeyed(executionID, lineageKey string, recs ...config.RecommendationRecord) ([]string, error) {
	r.t.Helper()
	r.mu.Lock()
	r.tokens = nil
	r.mu.Unlock()
	exec := &config.PurchaseExecution{
		ExecutionID:     executionID,
		IdempotencyKey:  lineageKey,
		PlanID:          "plan-x",
		StepNumber:      1,
		Source:          common.PurchaseSourceWeb,
		Recommendations: recs,
	}
	err := r.manager.executePurchase(context.Background(), exec)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.tokens...)
	sort.Strings(out)
	return out, err
}

// TestRetryAfterPlanGainsFirstAccountReusesSingleAccountToken is the issue #97
// regression guard, zero-to-N direction.
//
// The first attempt runs while the plan has no plan_accounts, so it takes the
// single-account path and buys under DeriveIdempotencyToken(K, 0). It is then
// reaped and marked failed although the commitment landed. An operator attaches
// the account to the plan and retries. The successor carries the same lineage
// key, and must buy under the SAME token so the provider dedupes; deriving the
// fan-out token DeriveIdempotencyToken(K+":acct-A", 0) instead is a token the
// provider has never seen, and the commitment is bought twice.
func TestRetryAfterPlanGainsFirstAccountReusesSingleAccountToken(t *testing.T) {
	r := newLineageRig(t)
	single := common.DeriveIdempotencyToken("lineage-97", 0)

	first, err := r.attempt("attempt-1")
	require.NoError(t, err)
	require.Equal(t, []string{single}, first,
		"the first attempt of a plan with no accounts must keep the pre-#97 single-account token byte-for-byte")

	r.setPlanAccounts(shapeTestAccountA)

	retry, err := r.attempt("attempt-2")
	require.NoError(t, err)
	assert.Equal(t, []string{single}, retry,
		"the retry must reuse the first attempt's token; a different token misses the provider's dedupe and double-buys (issue #97)")
}

// TestRetryAfterPlanLosesLastAccountBuysNothing is the issue #97 regression
// guard, N-to-zero direction.
//
// The first attempt fanned out and bought acct-A under
// DeriveIdempotencyToken(K+":acct-A", 0). The account is then removed from the
// plan and the run is retried. The single-account path would derive
// DeriveIdempotencyToken(K, 0), which the provider has never seen, and there is
// no plan account left to fan out to, so the only safe outcome is to refuse and
// buy nothing.
func TestRetryAfterPlanLosesLastAccountBuysNothing(t *testing.T) {
	r := newLineageRig(t)
	r.setPlanAccounts(shapeTestAccountA)

	first, err := r.attempt("attempt-1")
	require.NoError(t, err)
	require.Equal(t, []string{common.DeriveIdempotencyToken("lineage-97:acct-A", 0)}, first,
		"the first attempt of a plan with accounts must keep the pre-#97 fan-out token byte-for-byte")

	r.setPlanAccounts()

	retry, err := r.attempt("attempt-2")
	require.Error(t, err, "the retry must be refused, not run on a path the first attempt did not take")
	assert.Empty(t, retry, "nothing may be bought under a token the first attempt never used (issue #97)")
	var notSubmitted *beforeSubmissionError
	assert.ErrorAs(t, err, &notSubmitted, "the refusal happens before any provider call and must say so")
}

// TestUnchangedPlanKeepsItsTokensAcrossRetries pins backward compatibility: a
// lineage whose plan never crosses zero-to-N accounts derives exactly the
// tokens it derived before issue #97, on every attempt and on either path.
func TestUnchangedPlanKeepsItsTokensAcrossRetries(t *testing.T) {
	cases := []struct {
		name     string
		accounts []config.CloudAccount
		want     string
	}{
		{"no plan accounts: single-account path", nil, common.DeriveIdempotencyToken("lineage-97", 0)},
		{"plan accounts: fan-out path", []config.CloudAccount{shapeTestAccountA}, common.DeriveIdempotencyToken("lineage-97:acct-A", 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLineageRig(t)
			r.setPlanAccounts(tc.accounts...)
			for _, id := range []string{"attempt-1", "attempt-2"} {
				got, err := r.attempt(id)
				require.NoError(t, err)
				assert.Equal(t, []string{tc.want}, got, "%s must derive the pre-#97 token", id)
			}
		})
	}
}

// TestFanOutShapePinFailureBuysNothing: when the shape cannot be pinned the
// executor cannot know which tokens another attempt used, so it must fail
// closed before any provider call. On the single-account path the refusal is
// stamped as before-submission; on the fan-out it fails the account like a
// credential failure does.
func TestFanOutShapePinFailureBuysNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []config.CloudAccount
	}{
		{"single-account path", nil},
		{"fan-out path", []config.CloudAccount{shapeTestAccountA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLineageRig(t)
			r.setPlanAccounts(tc.accounts...)
			r.store.PinExecutionFanOutShapeFn = func(_ context.Context, _, _ string) (string, error) {
				return "", errors.New("db unavailable")
			}

			got, err := r.attempt("attempt-1")
			require.ErrorContains(t, err, "db unavailable")
			assert.Empty(t, got, "nothing may be bought when the lineage's shape cannot be pinned")
			if tc.accounts == nil {
				var notSubmitted *beforeSubmissionError
				assert.ErrorAs(t, err, &notSubmitted, "the refusal happens before any provider call and must say so")
			}
		})
	}
}

// TestRetryAfterPreSubmissionRefusalFollowsReconfiguredPlan: an attempt refused
// before any provider call derived no token that needs protecting, so it must
// not pin its shape. Here the plan has no accounts and the step's recs span two
// accounts, which the single-account path refuses (#1902). The operator then
// attaches both accounts so the step can fan out; the retry must fan out and
// buy each account once, not be bound to the single-account path and refused
// on every retry.
func TestRetryAfterPreSubmissionRefusalFollowsReconfiguredPlan(t *testing.T) {
	r := newLineageRig(t)
	recs := func() []config.RecommendationRecord {
		b := scopedTestRec("acct-B")
		b.ResourceType = "c5.large"
		return []config.RecommendationRecord{scopedTestRec("acct-A"), b}
	}

	first, err := r.attemptKeyed("attempt-1", "lineage-97", recs()...)
	require.Error(t, err, "the single-account path must refuse recs that span accounts")
	require.Empty(t, first)
	pinned, err := r.store.GetExecutionFanOutShape(context.Background(), "lineage-97")
	require.NoError(t, err)
	assert.Empty(t, pinned, "an attempt that never reached submission must not pin the lineage's shape")

	r.setPlanAccounts(shapeTestAccountA, shapeTestAccountB)

	retry, err := r.attemptKeyed("attempt-2", "lineage-97", recs()...)
	require.NoError(t, err)
	want := []string{
		common.DeriveIdempotencyToken("lineage-97:acct-A", 0),
		common.DeriveIdempotencyToken("lineage-97:acct-B", 0),
	}
	sort.Strings(want)
	assert.Equal(t, want, retry, "the retry must fan out across the newly attached accounts")
}

// TestColonLineageKeysAreNeverPinned: only root lineages are pinned. A
// scopeless per-account row ("<root key>:<account id>", issue #1537) must
// neither read nor write a pin under its own key, whichever path it takes, and
// a fan-out must pin its ROOT key, never a per-account one.
func TestColonLineageKeysAreNeverPinned(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []config.CloudAccount
	}{
		{"account still on the plan: re-attached single-account re-drive", []config.CloudAccount{shapeTestAccountA}},
		{"plan has no accounts: single-account path", nil},
	} {
		t.Run("scopeless per-account row, "+tc.name, func(t *testing.T) {
			r := newLineageRig(t)
			r.setPlanAccounts(tc.accounts...)

			got, err := r.attemptKeyed("successor", "root-lineage:acct-A", scopedTestRec("acct-A"))
			require.NoError(t, err)
			require.Equal(t, []string{common.DeriveIdempotencyToken("root-lineage:acct-A", 0)}, got)
			store := r.store
			store.AssertNotCalled(t, "GetExecutionFanOutShape")
			store.AssertNotCalled(t, "PinExecutionFanOutShape")
		})
	}

	t.Run("root fan-out pins the root key", func(t *testing.T) {
		r := newLineageRig(t)
		r.setPlanAccounts(shapeTestAccountA)
		var mu sync.Mutex
		var keys []string
		r.store.PinExecutionFanOutShapeFn = func(_ context.Context, key, shape string) (string, error) {
			mu.Lock()
			keys = append(keys, key)
			mu.Unlock()
			return shape, nil
		}

		_, err := r.attempt("attempt-1")
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, keys, "a fan-out that submits must pin its lineage")
		for _, k := range keys {
			assert.Equal(t, "lineage-97", k, "only the root lineage key may be pinned")
		}
	})
}

// TestConcurrentAttemptPinnedOtherShapeBuysNothing: the read said nothing was
// pinned, but by the time this attempt pins, a concurrent attempt of the same
// lineage pinned the other shape and submitted under it. This attempt must not
// submit under its own shape's tokens.
func TestConcurrentAttemptPinnedOtherShapeBuysNothing(t *testing.T) {
	r := newLineageRig(t)
	r.store.PinExecutionFanOutShapeFn = func(_ context.Context, _, _ string) (string, error) {
		return config.FanOutShapeMulti, nil
	}

	got, err := r.attempt("attempt-1")
	require.Error(t, err)
	assert.Empty(t, got, "nothing may be bought under the losing shape's tokens")
	var notSubmitted *beforeSubmissionError
	assert.ErrorAs(t, err, &notSubmitted)
}
