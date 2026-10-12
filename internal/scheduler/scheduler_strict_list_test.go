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

// The lenient ListRecommendations logs these and over-shows; the strict
// variant must hand the error back so a purchase never sizes off a read it
// could not complete.
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
			if name != "stored list" {
				lenient, lerr := s.ListRecommendations(context.Background(), config.RecommendationFilter{})
				require.NoError(t, lerr, "lenient path swallows the same failure")
				assert.NotEmpty(t, lenient)
			}
		})
	}
}
