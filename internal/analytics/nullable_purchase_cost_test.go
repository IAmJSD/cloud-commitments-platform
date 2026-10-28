package analytics

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectorUnknownUpfront(t *testing.T) {
	for _, costs := range [][]*float64{{nil}, {nil, new(float64(120))}, {new(float64(120)), nil}, {new(float64)}} {
		purchases := make([]config.PurchaseHistoryRecord, len(costs), len(costs)+1)
		unknown := false
		for i, cost := range costs {
			purchases[i] = activeRecord("aws", "ec2", "us-east-1", 1, 10, 0)
			purchases[i].UpfrontCost = cost
			unknown = unknown || cost == nil
		}
		other := activeRecord("aws", "ec2", "us-east-1", 1, 20, 240)
		other.AccountID = "other-account"
		purchases = append(purchases, other)
		groups, _, _, err := aggregatePurchases(context.Background(), purchases, time.Now())
		require.NoError(t, err)
		snapshots := buildSnapshots(groups, time.Now())
		require.Len(t, snapshots, 2)
		for _, snapshot := range snapshots {
			if snapshot.AccountID == other.AccountID {
				require.NotNil(t, snapshot.TotalCommitment)
				assert.Equal(t, 20.0, *snapshot.TotalCommitment)
				continue
			}
			assert.Equal(t, 10*float64(len(costs)), snapshot.TotalSavings)
			assert.Equal(t, len(costs), snapshot.Metadata["active_purchases"])
			body, err := json.Marshal(snapshot)
			require.NoError(t, err)
			if unknown {
				assert.Nil(t, snapshot.TotalCommitment)
				assert.Contains(t, string(body), `"total_commitment":null`)
			} else {
				require.NotNil(t, snapshot.TotalCommitment)
				assert.Zero(t, *snapshot.TotalCommitment)
				assert.Contains(t, string(body), `"total_commitment":0`)
			}
		}
	}
}

// TestCollectorUnknownMonthlyCost covers issue #1423: one nil MonthlyCost in a
// snapshot row makes total_usage unknown instead of a partial sum stored as
// known, while a row whose costs are all known (including a known 0) keeps its
// real total.
func TestCollectorUnknownMonthlyCost(t *testing.T) {
	cases := []struct {
		name  string
		costs []*float64
		want  *float64
	}{
		{"recurring then all-upfront", []*float64{new(float64(120)), nil}, nil},
		{"all-upfront then recurring", []*float64{nil, new(float64(120))}, nil},
		{"all unknown", []*float64{nil, nil}, nil},
		{"all known", []*float64{new(float64(120)), new(float64(30))}, new(float64(150))},
		{"known zero stays zero", []*float64{new(float64)}, new(float64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			purchases := make([]config.PurchaseHistoryRecord, len(tc.costs), len(tc.costs)+1)
			for i, cost := range tc.costs {
				purchases[i] = activeRecord("aws", "ec2", "us-east-1", 1, 10, 0)
				purchases[i].MonthlyCost = cost
			}
			other := activeRecord("aws", "ec2", "us-east-1", 1, 20, 240)
			other.AccountID = "other-account"
			other.MonthlyCost = new(float64(45))
			purchases = append(purchases, other)
			groups, _, _, err := aggregatePurchases(context.Background(), purchases, time.Now())
			require.NoError(t, err)
			snapshots := buildSnapshots(groups, time.Now())
			require.Len(t, snapshots, 2)
			for _, snapshot := range snapshots {
				if snapshot.AccountID == other.AccountID {
					require.NotNil(t, snapshot.TotalUsage, "an unknown cost must not leak into another row")
					assert.InDelta(t, 45.0, *snapshot.TotalUsage, 0.001)
					continue
				}
				assert.Equal(t, len(tc.costs), snapshot.Metadata["active_purchases"])
				if tc.want == nil {
					assert.Nil(t, snapshot.TotalUsage)
					continue
				}
				require.NotNil(t, snapshot.TotalUsage)
				assert.InDelta(t, *tc.want, *snapshot.TotalUsage, 0.001)
			}
		})
	}
}
