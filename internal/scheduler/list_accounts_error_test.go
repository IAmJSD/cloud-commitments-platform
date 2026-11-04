package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A failed account listing must fail the provider's collection. Treating it
// as "no accounts" sends the scheduler down the ambient-credential path and
// stores recommendations under the host identity (#721).
func TestCollectRecommendations_ListAccountsErrorFailsClosed(t *testing.T) {
	listErr := errors.New("connection reset by peer")
	tests := []struct {
		name    string
		env     map[string]string
		collect func(*Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error)
	}{
		{"aws", nil, func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
			return s.collectAWSRecommendations(context.Background(), &config.GlobalConfig{})
		}},
		{"azure", map[string]string{"AZURE_SUBSCRIPTION_ID": "sub-a"}, func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
			return s.collectAzureRecommendations(context.Background(), &config.GlobalConfig{})
		}},
		{"gcp", map[string]string{"GCP_PROJECT_ID": "proj-a"}, func(s *Scheduler) ([]config.RecommendationRecord, []string, *sweepGap, error) {
			return s.collectGCPRecommendations(context.Background(), &config.GlobalConfig{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("ListCloudAccounts", mock.Anything, mock.Anything).Return(nil, listErr)
			// No expectations: any provider creation means ambient credentials were used.
			factory := new(MockProviderFactory)
			s := &Scheduler{config: store, providerFactory: factory}

			recs, succeeded, gap, err := tt.collect(s)

			require.Error(t, err)
			assert.ErrorIs(t, err, listErr)
			assert.Empty(t, recs)
			assert.Empty(t, succeeded, "a failed listing must not make any account eligible for eviction")
			assert.Nil(t, gap, "a failed listing is a failure, not a partial sweep")
			factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}
