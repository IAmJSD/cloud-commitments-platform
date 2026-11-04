package api

import (
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

// Issue #723: once the frontend reads the list in pages, every row it shows
// comes from the paged response, so pages must carry the same top-level
// compute capacity (vcpu / memory_gb, #219) as the unpaged list. Both paged
// sources, the SQL page (setPage) and the Go scan (add), end in response.
func TestRecommendationsPageCollector_StampsComputeCapacity(t *testing.T) {
	details, err := common.MarshalServiceDetails(&common.ComputeDetails{
		InstanceType: "m5.2xlarge", Platform: "linux", Tenancy: "default", VCPU: 8, MemoryGB: 32,
	})
	require.NoError(t, err)
	rows := func() []config.RecommendationRecord {
		return []config.RecommendationRecord{
			{ID: "a-compute", Service: "ec2", Region: "us-east-1", Details: details},
			{ID: "b-database", Service: "rds", Region: "us-east-1", Details: json.RawMessage(`{"engine":"postgres"}`)},
		}
	}
	page := recommendationsPage{enabled: true, limit: 10}
	check := func(t *testing.T, resp *RecommendationsResponse) {
		t.Helper()
		require.Len(t, resp.Recommendations, 2)
		compute := resp.Recommendations[0]
		require.NotNil(t, compute.VCPU, "paged compute row must carry top-level vcpu")
		require.NotNil(t, compute.MemoryGB, "paged compute row must carry top-level memory_gb")
		require.Equal(t, 8, *compute.VCPU)
		require.Equal(t, float64(32), *compute.MemoryGB)
		require.Nil(t, resp.Recommendations[1].VCPU, "a non-compute row has no vcpu")
	}

	t.Run("scan", func(t *testing.T) {
		c := newRecommendationsPageCollector(page)
		require.NoError(t, c.add(rows()))
		resp, err := c.response(recommendationsPageByteBudget)
		require.NoError(t, err)
		check(t, resp)
	})
	t.Run("sql", func(t *testing.T) {
		c := newRecommendationsPageCollector(page)
		require.NoError(t, c.setPage(&config.RecommendationPage{Rows: rows(), TotalCount: 2}))
		resp, err := c.response(recommendationsPageByteBudget)
		require.NoError(t, err)
		check(t, resp)
	})
}
