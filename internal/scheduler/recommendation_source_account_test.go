package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

func TestConvertRecommendationsSourceAccount(t *testing.T) {
	t.Parallel()
	for _, cloud := range []string{"aws", "azure", "gcp"} {
		t.Run(cloud, func(t *testing.T) {
			s := &Scheduler{}
			input := []common.Recommendation{
				{Account: " SourceAbC/é\t", Service: common.ServiceEC2, Term: "1yr"},
				{Account: "sourceabc/e\u0301", Service: common.ServiceEC2, Term: "1yr"},
				{Service: common.ServiceEC2, Term: "1yr"},
			}
			records := s.convertRecommendations(input, cloud)
			require.Len(t, records, 3)
			input[0].Account = "changed"
			tagged := s.tagAccount(records, "11111111-1111-4111-8111-111111111111")
			require.Len(t, tagged, 3)
			for i, want := range []string{" SourceAbC/é\t", "sourceabc/e\u0301", ""} {
				raw, err := json.Marshal(tagged[i])
				require.NoError(t, err)
				var wire map[string]any
				require.NoError(t, json.Unmarshal(raw, &wire))
				require.Equal(t, "11111111-1111-4111-8111-111111111111", wire["cloud_account_id"])
				if want == "" {
					require.NotContains(t, wire, "source_account_id")
				} else {
					require.Equal(t, want, wire["source_account_id"])
				}
			}
		})
	}
}

func TestRecommendationSourceAccountLegacyJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"source_account_id":null}`} {
		var record config.RecommendationRecord
		require.NoError(t, json.Unmarshal([]byte(raw), &record))
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(encoded, &wire))
		require.NotContains(t, wire, "source_account_id")
	}
}
