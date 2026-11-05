package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

// A failed provider's own error text carries project and account identifiers
// and request IDs; the stored last_collection_error and the collect result
// hold only a category message.
func TestCollectRecommendations_StoresSummarizedProviderError(t *testing.T) {
	t.Setenv("GCP_PROJECT_ID", "proj-a")
	raw := fmt.Errorf("get GCP recommendations: %w", &googleapi.Error{
		Code:    403,
		Message: "Permission denied on resource project my-project-123456789012; request 11111111-2222-3333-4444-555555555555",
		Errors:  []googleapi.ErrorItem{{Reason: "forbidden", Message: "caller arn:aws:sts::123456789012:assumed-role/x/y"}},
	})
	s, store := newPartialSweepScheduler(t, "gcp", raw)
	expectScheduledCollection(store, "gcp")
	var stored string
	store.On("SetRecommendationsCollectionError", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { stored = args.String(1) }).Return(nil).Once()

	result, err := s.CollectRecommendations(context.Background(), "")

	require.NoError(t, err)
	for _, got := range []string{stored, result.FailedProviders["gcp"]} {
		for _, needle := range []string{"my-project-123456789012", "11111111-2222", "arn:aws", "123456789012", "googleapi"} {
			assert.NotContains(t, got, needle)
		}
	}
	assert.Equal(t, "gcp: the cloud provider refused the purchase: CUDly's role is not allowed to make it in this account", stored)
}
