package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	awsrecommendations "github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestAWSIncompleteSweep(t *testing.T) {
	incomplete := &awsrecommendations.IncompleteRecommendationsError{
		FailedDetails: 2, FailedScopes: 1, Causes: []error{errors.New("malformed quantity"), errors.New("API scope failed")},
	}
	for _, tc := range []struct {
		name     string
		err      error
		complete bool
		fatal    bool
	}{
		{name: "complete", complete: true},
		{name: "incomplete", err: incomplete},
		{name: "wrapped incomplete", err: fmt.Errorf("collect: %w", incomplete)},
		{name: "ordinary failure", err: errors.New("API unavailable"), fatal: true},
		{name: "canceled", err: context.Canceled, fatal: true},
		{name: "deadline", err: context.DeadlineExceeded, fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gap, err := tolerateIncompleteSweep("aws", tc.err)
			require.Equal(t, tc.complete || tc.fatal, gap == nil,
				"only a tolerated incomplete sweep carries a gap")
			if tc.fatal {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAWSIncompleteFallbackCannotAuthorizeEviction(t *testing.T) {
	incomplete := &awsrecommendations.IncompleteRecommendationsError{FailedDetails: 1, Causes: []error{errors.New("invalid quantity")}}
	for _, tc := range []struct {
		name     string
		firstErr error
		lastErr  error
	}{
		{name: "all-invalid then complete", firstErr: incomplete},
		{name: "empty then incomplete", lastErr: incomplete},
		{name: "both incomplete", firstErr: incomplete, lastErr: incomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := new(MockRecommendationsClient)
			client.On("GetAllRecommendations", mock.Anything).Return([]common.Recommendation{}, tc.firstErr).Once()
			for _, service := range []common.ServiceType{common.ServiceEC2, common.ServiceRDS, common.ServiceElastiCache, common.ServiceOpenSearch, common.ServiceRedshift, common.ServiceSavingsPlansAll} {
				var rows []common.Recommendation
				var lastErr error
				if service == common.ServiceRDS {
					rows = []common.Recommendation{{Service: common.ServiceRDS, Region: "us-east-1", ResourceType: "db.t3.medium", Count: 2,
						Term: "1yr", PaymentOption: "no-upfront"}}
					lastErr = tc.lastErr
				}
				client.On("GetRecommendations", mock.Anything, mock.MatchedBy(func(params *common.RecommendationParams) bool {
					return params.Service == service && params.Term == "1yr" && params.PaymentOption == "no-upfront" && params.LookbackPeriod == "30d"
				})).Return(rows, lastErr).Once()
			}
			prov := new(MockProvider)
			prov.On("GetRecommendationsClient", mock.Anything).Return(client, nil).Once()
			s := &Scheduler{}
			recs, gap, err := s.fetchAndConvert(context.Background(), prov, "aws", nil, "",
				&config.GlobalConfig{DefaultTerm: 1, DefaultPayment: "no-upfront", RecommendationsLookbackDays: 30})
			require.NoError(t, err)
			require.Len(t, recs, 1)
			require.NotNil(t, gap, "an incomplete primary or fallback fetch must leave the account incomplete")
			assert.Zero(t, gap.incompleteAccounts,
				"fetchAndConvert merges scope-level gaps; the account is counted once, by its caller")
			client.AssertExpectations(t)
			prov.AssertExpectations(t)
		})
	}
}

// A partially-swept account must never authorize stale-row eviction.
//
// UpsertRecommendations deletes an account's previous-cycle rows with
//
//	DELETE FROM recommendations
//	 WHERE collected_at < $1 AND (provider, account_key) IN (…)
//
// where the roster comes from the succeeded-account IDs these collect
// functions return. That predicate is keyed on (provider, account_key) --
// the CUDly account UUID -- but an Azure sweep fails one SUBSCRIPTION at a
// time. A sweep that never queried subscription B returns no rows for B, so
// listing the account as succeeded deletes B's previous rows: its savings
// opportunities vanish and the dashboard reads "nothing to buy" rather than
// "collection incomplete".
//
// The existing account-level tests cannot see this. They assert that a
// collection which returns an *error* is excluded, and a partial sweep
// deliberately returns data plus *PartialSubscriptionFailureError -- the one
// shape that is neither a clean success nor a failure. That is the whole
// defect, and why these tests drive the sub-account granularity directly.
//
// Note the assertions are on the roster, NOT on the recommendations: a
// partial sweep's rows must still be collected and upserted, so the
// subscriptions that did succeed get fresh data. Only the authority to
// DELETE is withheld. Failing the account instead would blank a whole
// account's dashboard over one flaky subscription, which is the opposite
// failure and the invariant SuccessfulCollect exists to protect.

// partialSweepErr builds the error the Azure fan-out returns when some
// subscriptions could not be queried. Mirrors the real construction site,
// providers/azure/recommendations_multi_subscription.go.
func partialSweepErr(failedSubscriptionIDs ...string) *azureprovider.PartialSubscriptionFailureError {
	failures := make([]azureprovider.SubscriptionFailure, 0, len(failedSubscriptionIDs))
	for _, id := range failedSubscriptionIDs {
		failures = append(failures, azureprovider.SubscriptionFailure{
			SubscriptionID: id,
			Err:            errors.New("403 authorization failed"),
		})
	}
	return &azureprovider.PartialSubscriptionFailureError{
		Attempted: len(failedSubscriptionIDs) + 1,
		Succeeded: 1,
		Failed:    failures,
	}
}

// partialSweepCase is one provider's ambient collection path. Every case
// used to drive Azure only, so the GCP and AWS wiring of the gap returned by
// tolerateIncompleteSweep was uncovered (audit finding A16-007): a partial
// GCP sweep could enter the eviction roster with the whole suite green.
type partialSweepCase struct {
	provider   string
	env        map[string]string
	partialErr error
	collect    func(*Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error)
}

func partialSweepCases() []partialSweepCase {
	return []partialSweepCase{
		{
			provider: "aws",
			partialErr: &awsrecommendations.IncompleteRecommendationsError{
				FailedScopes: 2, Causes: []error{errors.New("scope one failed"), errors.New("scope two failed")},
			},
			collect: func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
				return s.collectAWSRecommendations(context.Background(), &config.GlobalConfig{})
			},
		},
		{
			provider:   "azure",
			env:        map[string]string{"AZURE_SUBSCRIPTION_ID": "sub-a"},
			partialErr: partialSweepErr("sub-b", "sub-c"),
			collect: func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
				return s.collectAzureRecommendations(context.Background(), &config.GlobalConfig{})
			},
		},
		{
			provider:   "gcp",
			env:        map[string]string{"GCP_PROJECT_ID": "proj-a"},
			partialErr: partialSweepErr("proj-b", "proj-c"),
			collect: func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
				return s.collectGCPRecommendations(context.Background(), &config.GlobalConfig{})
			},
		},
	}
}

// partialSweepRec is the one recommendation the surviving scope returns.
func partialSweepRec() common.Recommendation {
	return common.Recommendation{
		Service:       common.ServiceEC2,
		Region:        "westeurope",
		ResourceType:  "Standard_D2s_v3",
		Count:         3,
		Term:          "1yr",
		PaymentOption: "all-upfront",
	}
}

// newPartialSweepScheduler wires a Scheduler whose providerName provider
// returns one recommendation plus sweepErr, exercising the ambient path (no
// registered accounts; AZURE_SUBSCRIPTION_ID / GCP_PROJECT_ID set by the
// caller). It returns the store so callers can add expectations.
func newPartialSweepScheduler(t *testing.T, providerName string, sweepErr error) (*Scheduler, *MockConfigStore) {
	t.Helper()

	recClient := new(MockRecommendationsClient)
	t.Cleanup(func() { recClient.AssertExpectations(t) })
	recClient.On("GetAllRecommendations", mock.Anything).
		Return([]common.Recommendation{partialSweepRec()}, sweepErr).Once()

	prov := new(MockProvider)
	t.Cleanup(func() { prov.AssertExpectations(t) })
	prov.On("GetRecommendationsClient", mock.Anything).Return(recClient, nil).Once()

	factory := new(MockProviderFactory)
	t.Cleanup(func() { factory.AssertExpectations(t) })
	factory.On("CreateAndValidateProvider", mock.Anything, providerName, mock.Anything).
		Return(prov, nil).Once()

	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	// No registered accounts -> ambient path.
	store.On("ListCloudAccounts", mock.Anything, mock.Anything).
		Return([]config.CloudAccount{}, nil).Maybe()
	// Ambient account resolution is best-effort; leave it unresolved so the
	// roster carries the nil-CloudAccountID sentinel and the assertion is
	// about roster membership rather than which ID it holds.
	store.On("GetCloudAccountByExternalID", mock.Anything, providerName, mock.Anything).
		Return(nil, nil).Maybe()

	return &Scheduler{config: store, providerFactory: factory}, store
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// TestCollectProvider_PartialSweepIsNotEvictionEligible is the regression
// for #1652, now per provider. It fails on the pre-#1652 code, where
// tolerateIncompleteSweep returns nil and the account is reported as fully
// succeeded, and on any provider whose collector drops the gap (#87).
func TestCollectProvider_PartialSweepIsNotEvictionEligible(t *testing.T) {
	for _, tc := range partialSweepCases() {
		t.Run(tc.provider, func(t *testing.T) {
			setEnv(t, tc.env)
			s, _ := newPartialSweepScheduler(t, tc.provider, tc.partialErr)

			recs, succeededAccountIDs, gap, err := tc.collect(s)

			require.NoError(t, err,
				"a partial sweep must not fail the account: one flaky subscription cannot blank the whole account's dashboard")
			assert.NotEmpty(t, recs,
				"the subscriptions that DID succeed must still contribute rows to be upserted")
			assert.Emptyf(t, succeededAccountIDs,
				"a partial sweep must not enter the eviction roster, got %#v: 2 scope(s) were never "+
					"queried, so their previous-cycle rows would be deleted by the (provider, account_key) DELETE",
				succeededAccountIDs)
			require.NotNil(t, gap, "the partial sweep must reach the provider outcome so it can be persisted (#87)")
			assert.Equal(t, 1, gap.incompleteAccounts, "the ambient identity is one incomplete account")
			assert.Equal(t, 2, gap.failedScopes)
		})
	}
}

// TestCollectProvider_CompleteSweepStaysEvictionEligible is the control. The
// fix must withhold eviction ONLY for partial sweeps -- if a clean sweep
// also stopped evicting, stale rows would accumulate forever and rows for
// genuinely-gone resources would never disappear. Under-evicting is a
// quieter bug than over-evicting, so it gets its own assertion.
func TestCollectProvider_CompleteSweepStaysEvictionEligible(t *testing.T) {
	for _, tc := range partialSweepCases() {
		t.Run(tc.provider, func(t *testing.T) {
			setEnv(t, tc.env)
			s, _ := newPartialSweepScheduler(t, tc.provider, nil)

			recs, succeededAccountIDs, gap, err := tc.collect(s)

			require.NoError(t, err)
			assert.NotEmpty(t, recs)
			assert.NotEmpty(t, succeededAccountIDs,
				"a complete sweep must still authorize eviction, otherwise stale rows accumulate forever")
			assert.Nil(t, gap, "a complete sweep must not be marked partial")
		})
	}
}

// TestFanOutPerAccount_PartialSweepNotInSucceededAccountIDs covers the
// registered-account path, which is the common production shape and does not
// go through the ambient collector the tests above drive. It is the partial
// sibling of TestFanOutPerAccount_FailedCollectionNotInSucceededAccountIDs:
// that one pins the FAILED account, this one pins the account that returned
// data from an incomplete sweep -- neither a clean success nor a failure, and
// the case the account-level tests never exercised.
func TestFanOutPerAccount_PartialSweepNotInSucceededAccountIDs(t *testing.T) {
	accounts := []config.CloudAccount{
		{ID: "acct-complete", Name: "acct-complete", ExternalID: "e-ok"},
		{ID: "acct-partial", Name: "acct-partial", ExternalID: "e-partial"},
	}
	fn := func(_ context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
		// Both return rows; only one swept every subscription.
		var gap *sweepGap
		if acct.ID != "acct-complete" {
			gap = gapFromPartialSubscriptions(partialSweepErr("sub-b", "sub-c"))
		}
		return []config.RecommendationRecord{{ID: acct.ID, Provider: "azure"}}, gap, nil
	}

	recs, outcome := fanOutPerAccount(context.Background(), "Azure", accounts, fn)

	assert.Len(t, recs, 2,
		"a partial sweep's rows must still be collected and upserted -- only eviction is withheld")
	assert.Equal(t, []string{"acct-complete"}, outcome.SucceededAccountIDs,
		"only the fully-swept account may authorize stale-row eviction")
	assert.Equal(t, []string{"acct-partial"}, outcome.IncompleteAccountIDs)
	assert.Equal(t, 2, outcome.SucceededCount,
		"a partial sweep still counts as a success, so one flaky subscription cannot trip the all-accounts-failed guard")
	assert.Zero(t, outcome.FailedCount)
	require.NotNil(t, outcome.Gap, "the partial account's gap must reach the provider outcome (#87)")
	assert.Equal(t, 1, outcome.Gap.incompleteAccounts)
	assert.Equal(t, 3, outcome.Gap.attemptedScopes)
	assert.Equal(t, 2, outcome.Gap.failedScopes)
}

// TestCollectProvider_HardSweepErrorStillFailsTheAccount pins that the fix
// did not soften genuine failures into partial ones: a non-partial error must
// still propagate, keeping the account out of the roster via the error path.
func TestCollectProvider_HardSweepErrorStillFailsTheAccount(t *testing.T) {
	for _, tc := range partialSweepCases() {
		t.Run(tc.provider, func(t *testing.T) {
			setEnv(t, tc.env)
			s, _ := newPartialSweepScheduler(t, tc.provider, errors.New("429 too many requests"))

			_, succeededAccountIDs, gap, err := tc.collect(s)

			require.Error(t, err, "a hard collection error must still fail the account")
			assert.Empty(t, succeededAccountIDs)
			assert.Nil(t, gap, "a hard failure is reported as an error, never as a partial sweep")
		})
	}
}
