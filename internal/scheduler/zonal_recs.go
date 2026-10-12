package scheduler

import (
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// zonalAvailabilityZone returns the AZ of a zonal EC2 recommendation, "" for
// regional recs and every non-compute service (issue #792).
func zonalAvailabilityZone(details common.ServiceDetails) string {
	if cd, ok := details.(*common.ComputeDetails); ok && cd != nil {
		return strings.TrimSpace(cd.AvailabilityZone)
	}
	return ""
}

// dropForeignZonalEC2 removes zonal EC2 recs that Cost Explorer computed for
// a member account other than the one being collected. AZ names map to
// different physical zones per account, and the record is stamped with the
// collecting account, so such a rec would be bought in the wrong zone
// (issue #792, F4). Recs with an empty Account are kept; an empty
// expectedAccount (identity unknown, or a non-AWS provider) disables the
// check. The drop is logged and does not mark the collection incomplete.
func dropForeignZonalEC2(recs []common.Recommendation, expectedAccount string) []common.Recommendation {
	kept, dropped := partitionForeignZonalEC2(recs, expectedAccount)
	if dropped > 0 {
		logging.Warnf("dropped %d zonal EC2 recommendation(s) computed for a member account other than %s: AZ names are per account and cannot be mapped safely", dropped, expectedAccount)
	}
	return kept
}

// partitionForeignZonalEC2 returns the recs to keep and the number dropped.
func partitionForeignZonalEC2(recs []common.Recommendation, expectedAccount string) (kept []common.Recommendation, dropped int) {
	if expectedAccount == "" {
		return recs, 0
	}
	kept = recs[:0:0]
	for i := range recs {
		r := recs[i]
		if r.Account != "" && r.Account != expectedAccount && zonalAvailabilityZone(r.Details) != "" {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}
