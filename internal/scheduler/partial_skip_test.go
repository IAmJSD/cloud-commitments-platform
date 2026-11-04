package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipErr stands in for the GCP provider's unexported partial-result error:
// the scheduler must match the Partial() method, not the type (issue #1472).
type skipErr struct {
	msg     string
	partial bool
	count   int
	reasons []string
}

func (e *skipErr) Error() string         { return e.msg }
func (e *skipErr) Partial() bool         { return e.partial }
func (e *skipErr) SkippedCount() int     { return e.count }
func (e *skipErr) SkipReasons() []string { return e.reasons }

const skipMsg = "gcp recommendations: 3 recommendation(s) skipped: computeengine us-east1: non-USD; cloudsql us-east1: bad plan"

func gcpAmbient(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	return s.collectGCPRecommendations(context.Background(), &config.GlobalConfig{})
}

func TestGCPPartialResult_KeepsRecsAndRecordsSkips(t *testing.T) {
	t.Setenv("GCP_PROJECT_ID", "proj-a")
	s, _ := newPartialSweepScheduler(t, "gcp", fmt.Errorf("collect: %w", &skipErr{msg: skipMsg, partial: true, count: 3}))

	recs, roster, gap, err := gcpAmbient(s)

	require.NoError(t, err, "a partial GCP result must not fail the collection run")
	assert.NotEmpty(t, recs, "the recommendations that converted must be kept")
	require.NotNil(t, gap, "the skip must be recorded for the user")
	assert.Equal(t, 3, gap.skippedRecommendations)
	assert.Equal(t, 3, gap.toProviderSweep("gcp").SkippedRecommendations)
	assert.NotEmpty(t, roster,
		"skipped recommendations are invalid, so the account stays eviction-eligible and their stale rows go")
	assert.Zero(t, gap.forAccount().incompleteAccounts, "a skip is not an incomplete account")
}

func TestGCPPartialResult_NonPartialErrorsStayHard(t *testing.T) {
	for name, err := range map[string]error{
		"plain error":        errors.New("boom"),
		"Partial() == false": &skipErr{msg: skipMsg, partial: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GCP_PROJECT_ID", "proj-a")
			s, _ := newPartialSweepScheduler(t, "gcp", err)

			recs, roster, gap, gotErr := gcpAmbient(s)

			require.Error(t, gotErr, "a non-partial error must fail the run")
			assert.Empty(t, recs)
			assert.Empty(t, roster)
			assert.Nil(t, gap)
		})
	}
}

func TestGCPPartialResult_FanOutKeepsAccountEvictable(t *testing.T) {
	accounts := []config.CloudAccount{{ID: "a1", Name: "a1", ExternalID: "p1"}, {ID: "a2", Name: "a2", ExternalID: "p2"}}
	fn := func(_ context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
		gap, err := tolerateIncompleteSweep("gcp", &skipErr{msg: skipMsg, partial: true, count: 3})
		if acct.ID == "a2" {
			// A lost project in the same account hold back eviction as before.
			gap = mergeSweepGaps(gap, gapFromPartialSubscriptions(partialSweepErr("p-x")))
		}
		return []config.RecommendationRecord{{ID: acct.ID}}, gap, err
	}

	recs, outcome := fanOutPerAccount(context.Background(), "GCP", accounts, fn)

	assert.Len(t, recs, 2)
	assert.Equal(t, []string{"a1"}, outcome.SucceededAccountIDs)
	assert.Equal(t, []string{"a2"}, outcome.IncompleteAccountIDs)
	require.NotNil(t, outcome.Gap)
	assert.Equal(t, 6, outcome.Gap.skippedRecommendations)
	assert.Equal(t, 1, outcome.Gap.incompleteAccounts)
}

// The count comes from SkippedCount(), never from the message text.
func TestGCPPartialResult_CountComesFromTypedMethod(t *testing.T) {
	gap, err := tolerateIncompleteSweep("gcp", &skipErr{msg: "something skipped", partial: true, count: 5})
	require.NoError(t, err)
	require.NotNil(t, gap)
	assert.Equal(t, 5, gap.skippedRecommendations)
}

// A mixed Azure sweep (lost subscriptions plus recommendations dropped on the
// reachable ones) reports the skipped count for the user warning, but the lost
// subscriptions still hold stale-row eviction back.
func TestAzureMixedSweep_RecordsSkipsAndHoldsEviction(t *testing.T) {
	failure := azureprovider.NewPartialSubscriptionFailure(2,
		[]azureprovider.SubscriptionFailure{{SubscriptionID: "sub-b", Err: errors.New("throttled")}},
		[]string{"sub-a: non-USD", "sub-a: bad term"})

	gap, err := tolerateIncompleteSweep("azure", failure)

	require.NoError(t, err)
	require.NotNil(t, gap)
	assert.Equal(t, 2, gap.skippedRecommendations)
	assert.Equal(t, 2, gap.toProviderSweep("azure").SkippedRecommendations)
	assert.True(t, gap.holdsEviction(), "lost subscriptions still hold stale-row eviction")
	assert.Equal(t, 1, gap.forAccount().incompleteAccounts)
}

func TestAzureFailureWithoutSkips_RecordsNoSkippedCount(t *testing.T) {
	failure := azureprovider.NewPartialSubscriptionFailure(2,
		[]azureprovider.SubscriptionFailure{{SubscriptionID: "sub-b", Err: errors.New("throttled")}}, nil)

	gap, err := tolerateIncompleteSweep("azure", failure)

	require.NoError(t, err)
	require.NotNil(t, gap)
	assert.Zero(t, gap.skippedRecommendations)
	assert.True(t, gap.holdsEviction())
}

func TestHasPricingOutageSkip(t *testing.T) {
	for name, tc := range map[string]struct {
		reasons []string
		want    bool
	}{
		"first failed row":                {[]string{"advisor: azure advisor: advisor compute reservation price: get: retail prices fetch failed"}, true},
		"later rows":                      {[]string{"advisor: non-USD", "advisor: price lookup unavailable: x"}, true},
		"invalid recommendation":          {[]string{"advisor: row is in EUR, not USD", "computeengine us-east1: non-USD"}, false},
		"mid-string mention":              {[]string{"computeengine: price lookup unavailable: x"}, false},
		"truncated walk is not an outage": {[]string{"advisor: azure advisor: recommendation list is incomplete"}, false},
		"truncated walk plus outage":      {[]string{"advisor: azure advisor: recommendation list is incomplete", "advisor: azure advisor: get: retail prices fetch failed"}, true},
		"prefix mid-string":               {[]string{"sub-1: advisor: price lookup unavailable: x"}, false},
		"none":                            {nil, false},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, tc.want, hasPricingOutageSkip(tc.reasons)) })
	}
}

// A pricing outage holds eviction; ordinary invalid-row skips do not.
func TestTolerateSkipped_PricingOutageHoldsEviction(t *testing.T) {
	gap, err := tolerateIncompleteSweep("azure", &skipErr{partial: true, count: 1,
		reasons: []string{"advisor: price lookup unavailable: x"}})
	require.NoError(t, err)
	assert.True(t, gap.holdsEviction())
	assert.Equal(t, 1, gap.skippedRecommendations)

	gap, err = tolerateIncompleteSweep("azure", &skipErr{partial: true, count: 1, reasons: []string{"advisor: non-USD"}})
	require.NoError(t, err)
	assert.False(t, gap.holdsEviction())
}

// The held account's platform ID travels with the gap through the fan-out;
// accounts whose skips are ordinary invalid rows are not held.
func TestPricingOutageGap_FanOutRecordsHeldAccounts(t *testing.T) {
	accounts := []config.CloudAccount{{ID: "held", Name: "held"}, {ID: "plain", Name: "plain"}}
	fn := func(_ context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
		reason := "advisor: non-USD"
		if acct.ID == "held" {
			reason = "advisor: price lookup unavailable: x"
		}
		gap, err := tolerateIncompleteSweep("azure", &skipErr{partial: true, count: 1, reasons: []string{reason}})
		return []config.RecommendationRecord{{ID: acct.ID}}, gap, err
	}

	_, outcome := fanOutPerAccount(context.Background(), "Azure", accounts, fn)

	require.NotNil(t, outcome.Gap)
	sweep := outcome.Gap.toProviderSweep("azure")
	assert.Equal(t, []string{"held"}, sweep.HeldAccountIDs)
	assert.Equal(t, 1, sweep.IncompleteAccounts)
	assert.Equal(t, 1, sweep.Reasons[config.PartialReasonPricingUnavailable])
	assert.Equal(t, []string{"held"}, outcome.IncompleteAccountIDs)
	assert.Equal(t, []string{"plain"}, outcome.SucceededAccountIDs)
}
