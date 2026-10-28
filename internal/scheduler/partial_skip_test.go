package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipErr stands in for the GCP provider's unexported partial-result error:
// the scheduler must match the Partial() method, not the type (issue #1472).
type skipErr struct {
	msg     string
	partial bool
}

func (e *skipErr) Error() string { return e.msg }
func (e *skipErr) Partial() bool { return e.partial }

const skipMsg = "gcp recommendations: 3 recommendation(s) skipped: computeengine us-east1: non-USD; cloudsql us-east1: bad plan"

func gcpAmbient(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	return s.collectGCPRecommendations(context.Background(), &config.GlobalConfig{})
}

func TestGCPPartialResult_KeepsRecsAndRecordsSkips(t *testing.T) {
	t.Setenv("GCP_PROJECT_ID", "proj-a")
	s, _ := newPartialSweepScheduler(t, "gcp", fmt.Errorf("collect: %w", &skipErr{msg: skipMsg, partial: true}))

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
		gap, err := tolerateIncompleteSweep("gcp", &skipErr{msg: skipMsg, partial: true})
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

func TestGCPPartialResult_UnparsableCountStillRecorded(t *testing.T) {
	gap, err := tolerateIncompleteSweep("gcp", &skipErr{msg: "something skipped", partial: true})
	require.NoError(t, err)
	require.NotNil(t, gap)
	assert.Zero(t, gap.skippedRecommendations)
}
