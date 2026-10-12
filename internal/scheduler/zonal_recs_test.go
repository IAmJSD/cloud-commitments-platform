package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func zonalRec(account, az string) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Account: account, Service: common.ServiceEC2, Region: "us-east-1",
		ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.ComputeDetails{InstanceType: "m5.large", Platform: "Linux/UNIX", Tenancy: "default", Scope: "availability-zone", AvailabilityZone: az},
	}
}

// Two zonal recs for the same type and term that differ only by AZ must get
// distinct IDs and carry the AZ at the top level and inside Details (#792).
func TestConvertRecommendations_ZonalRecsKeepAZAndGetDistinctIDs(t *testing.T) {
	s := &Scheduler{}
	out := s.convertRecommendations([]common.Recommendation{zonalRec("", "us-east-1a"), zonalRec("", "us-east-1b")}, "aws")
	require.Len(t, out, 2)
	assert.Equal(t, "us-east-1a", out[0].AvailabilityZone)
	assert.Equal(t, "us-east-1b", out[1].AvailabilityZone)
	assert.NotEqual(t, out[0].ID, out[1].ID)
	var d common.ComputeDetails
	require.NoError(t, json.Unmarshal(out[1].Details, &d))
	assert.Equal(t, "us-east-1b", d.AvailabilityZone)
}

func TestConvertRecommendations_RegionalRecIDUnchanged(t *testing.T) {
	s := &Scheduler{}
	r := zonalRec("", "")
	r.Details.(*common.ComputeDetails).Scope = "region"
	out := s.convertRecommendations([]common.Recommendation{r}, "aws")
	require.Len(t, out, 1)
	assert.Empty(t, out[0].AvailabilityZone)
	assert.Equal(t, "aws||ec2|us-east-1|m5.large||1|all-upfront", out[0].ID)
}

func TestDropForeignZonalEC2(t *testing.T) {
	regional := zonalRec("222222222222", "")
	recs := []common.Recommendation{
		zonalRec("111111111111", "us-east-1a"), // own account: kept
		zonalRec("", "us-east-1b"),             // no account: kept
		zonalRec("222222222222", "us-east-1c"), // member account zonal: dropped
		regional,                               // member account regional: kept
	}
	got := dropForeignZonalEC2(recs, "111111111111")
	require.Len(t, got, 3)
	assert.Equal(t, "us-east-1a", zonalAvailabilityZone(got[0].Details))
	assert.Equal(t, "us-east-1b", zonalAvailabilityZone(got[1].Details))
	assert.Equal(t, "", zonalAvailabilityZone(got[2].Details))

	assert.Len(t, dropForeignZonalEC2(recs, ""), 4, "unknown collecting identity disables the check")
}
