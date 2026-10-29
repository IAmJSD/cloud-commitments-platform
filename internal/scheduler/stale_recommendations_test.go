package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestStaleRecommendationRetentionDaysFromEnv pins the parse rules: unset and
// anything that is not a positive whole number of days fall back to the
// default, so a typo can never become a zero-day retention.
func TestStaleRecommendationRetentionDaysFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", config.DefaultStaleRecommendationRetentionDays},
		{"valid", "45", 45},
		{"surrounding space", " 7 ", 7},
		{"one day", "1", 1},
		{"zero", "0", config.DefaultStaleRecommendationRetentionDays},
		{"negative", "-3", config.DefaultStaleRecommendationRetentionDays},
		{"duration syntax", "720h", config.DefaultStaleRecommendationRetentionDays},
		{"garbage", "thirty", config.DefaultStaleRecommendationRetentionDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(staleRecommendationRetentionEnvVar, tc.raw)
			assert.Equal(t, tc.want, staleRecommendationRetentionDaysFromEnv())
		})
	}
}

// The default is 30 days: about 30 missed daily collections (#1580).
func TestDefaultStaleRecommendationRetentionDays(t *testing.T) {
	assert.Equal(t, 30, config.DefaultStaleRecommendationRetentionDays)
}

// A persisted collection prunes stale pairs with the configured retention.
func TestScheduler_persistCollection_PrunesStalePairs(t *testing.T) {
	t.Setenv(staleRecommendationRetentionEnvVar, "")
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	recs := []config.RecommendationRecord{{ID: "r1", Provider: "aws"}}
	successful := []config.SuccessfulCollect{{Provider: "aws"}}
	mockStore.On("UpsertRecommendations", ctx, mock.Anything, recs, successful).Return(nil)
	mockStore.On("PruneStaleRecommendations", ctx, config.DefaultStaleRecommendationRetentionDays).Return(int64(4), nil)

	s := &Scheduler{config: mockStore}
	s.persistCollection(ctx, recs, successful, nil, nil)

	mockStore.AssertExpectations(t)
	mockStore.AssertNumberOfCalls(t, "PruneStaleRecommendations", 1)
}

// The env override reaches the store.
func TestScheduler_persistCollection_PruneUsesEnvRetention(t *testing.T) {
	t.Setenv(staleRecommendationRetentionEnvVar, "60")
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockStore.On("UpsertRecommendations", ctx, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mockStore.On("PruneStaleRecommendations", ctx, 60).Return(int64(0), nil)

	s := &Scheduler{config: mockStore}
	s.persistCollection(ctx, nil, nil, nil, nil)

	mockStore.AssertExpectations(t)
}

// A prune failure is logged and does not undo or fail the collection: the
// partial marker is still recorded before it and nothing else is called after.
func TestScheduler_persistCollection_PruneErrorIsBestEffort(t *testing.T) {
	t.Setenv(staleRecommendationRetentionEnvVar, "")
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	partial := &config.RecommendationsCollectionPartial{}
	mockStore.On("UpsertRecommendations", ctx, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mockStore.On("SetRecommendationsCollectionPartial", ctx, partial).Return(nil)
	mockStore.On("PruneStaleRecommendations", ctx, config.DefaultStaleRecommendationRetentionDays).
		Return(int64(0), errors.New("lock timeout"))

	s := &Scheduler{config: mockStore}
	s.persistCollection(ctx, nil, nil, nil, partial)

	mockStore.AssertExpectations(t)
	mockStore.AssertNotCalled(t, "SetRecommendationsCollectionError", mock.Anything, mock.Anything)
}

// A failed write skips the prune: the database is the thing failing, and a
// run that wrote nothing has no fresher data to justify deleting older rows.
func TestScheduler_persistCollection_UpsertFailureSkipsPrune(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockStore.On("UpsertRecommendations", ctx, mock.Anything, mock.Anything, mock.Anything).Return(errors.New("db down"))
	mockStore.On("SetRecommendationsCollectionError", ctx, mock.Anything).Return(nil)

	s := &Scheduler{config: mockStore}
	s.persistCollection(ctx, nil, nil, nil, nil)

	mockStore.AssertNotCalled(t, "PruneStaleRecommendations")
}
