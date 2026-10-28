package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture/mock evidence: in-memory fakes, no database.
type strictStore struct {
	mockSuppressionStore
	listErr, supErr, overrideErr error
}

func (s *strictStore) ListStoredRecommendations(ctx context.Context, f config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.mockSuppressionStore.ListStoredRecommendations(ctx, f)
}
func (s *strictStore) ListActiveSuppressions(ctx context.Context) ([]config.PurchaseSuppression, error) {
	if s.supErr != nil {
		return nil, s.supErr
	}
	return s.mockSuppressionStore.ListActiveSuppressions(ctx)
}
func (s *strictStore) GetServiceConfig(ctx context.Context, p, svc string) (*config.ServiceConfig, error) {
	if s.overrideErr != nil {
		return nil, s.overrideErr
	}
	return s.mockSuppressionStore.GetServiceConfig(ctx, p, svc)
}

func strictFixture() *strictStore {
	return &strictStore{mockSuppressionStore: mockSuppressionStore{
		recs: []config.RecommendationRecord{{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", Count: 5, CloudAccountID: strPtr("acct-1")}},
		sups: []config.PurchaseSuppression{{ExecutionID: "e1", AccountID: "acct-1", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "t4g.nano", SuppressedCount: 3, ExpiresAt: time.Now().Add(time.Hour)}},
	}}
}

func TestListRecommendationsStrictAppliesSuppressions(t *testing.T) {
	s := &Scheduler{config: strictFixture()}
	recs, err := s.ListRecommendationsStrict(context.Background(), config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 2, recs[0].Count)
}

// The strict variant must hand every read error back so a purchase never
// sizes off a read it could not complete. The lenient ListRecommendations
// differs only on an override failure, which it logs and over-shows; a
// suppression failure fails it too (issue #111), since showing a covered
// commitment at its full count invites buying it a second time.
func TestListRecommendationsStrictReturnsReadErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(*strictStore){
		"stored list":  func(s *strictStore) { s.listErr = boom },
		"suppressions": func(s *strictStore) { s.supErr = boom },
		"overrides":    func(s *strictStore) { s.overrideErr = boom },
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			st := strictFixture()
			fail(st)
			s := &Scheduler{config: st}
			recs, err := s.ListRecommendationsStrict(context.Background(), config.RecommendationFilter{})
			require.ErrorIs(t, err, boom)
			assert.Nil(t, recs)
			lenient, lerr := s.ListRecommendations(context.Background(), config.RecommendationFilter{})
			if name == "overrides" {
				require.NoError(t, lerr, "lenient path swallows an override failure")
				assert.NotEmpty(t, lenient)
				return
			}
			require.ErrorIs(t, lerr, boom, "lenient path fails closed on this read too")
			assert.Nil(t, lenient)
		})
	}
}

// TestListRecommendations_SuppressionReadFailureFailsClosed (issue #111): a
// suppression read failure must fail the read rather than return the covered
// rec at its full pre-purchase count with no "recently purchased" marker.
func TestListRecommendations_SuppressionReadFailureFailsClosed(t *testing.T) {
	boom := errors.New("suppressions down")
	st := strictFixture()
	st.supErr = boom
	s := &Scheduler{config: st}

	recs, err := s.ListRecommendations(context.Background(), config.RecommendationFilter{})

	require.ErrorIs(t, err, boom)
	assert.Nil(t, recs, "no rec may be returned unsuppressed")
}

// TestGetRecommendationByID_SuppressionReadFailureFailsClosed (issue #111):
// the detail read fails closed the same way, instead of serving the rec as
// un-suppressed.
func TestGetRecommendationByID_SuppressionReadFailureFailsClosed(t *testing.T) {
	boom := errors.New("suppressions down")
	st := strictFixture()
	st.supErr = boom
	s := &Scheduler{config: st}

	rec, hiddenBy, err := s.GetRecommendationByID(context.Background(), "r1")

	require.ErrorIs(t, err, boom)
	assert.Nil(t, rec, "the rec must not be served un-suppressed")
	assert.Nil(t, hiddenBy)
}

// TestApplySuppressions_ReadFailureReturnsNoRecs pins the helper itself: on a
// read error it hands back no recs, so a caller that ignored the error would
// still show nothing rather than the un-suppressed list.
func TestApplySuppressions_ReadFailureReturnsNoRecs(t *testing.T) {
	st := strictFixture()
	st.supErr = errors.New("suppressions down")
	s := &Scheduler{config: st}

	recs, err := s.applySuppressions(context.Background(), st.recs)

	require.Error(t, err)
	assert.Nil(t, recs)
}
