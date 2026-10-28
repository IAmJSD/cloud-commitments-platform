package purchase

import (
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/savingsplans"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// rejectUnsupportedPartition refuses a Savings Plans recommendation for an
// AWS China (aws-cn) account before any provider is built or AWS is called
// (issue #1194). AWS bills aws-cn Savings Plans in CNY and the purchase path
// is USD-only. The library refuses too, but only when a region it sees is
// cn-*; an SP rec can carry no region, and the account's role ARN is the
// reliable signal here. The error wraps savingsplans.ErrUnsupportedPartition
// so callers can match it with errors.Is.
//
// awsPartition is the target account's partition (CloudAccount.AWSPartition),
// which is only set for AWS accounts; "" (no target account, or no role ARN)
// leaves the library guard as the only check.
func (m *Manager) rejectUnsupportedPartition(rec *config.RecommendationRecord, awsPartition string) error {
	if awsPartition != config.AWSPartitionChina {
		return nil
	}
	if !common.IsSavingsPlan(m.mapServiceType(rec.Service)) {
		return nil
	}
	return fmt.Errorf("savings plans purchase refused for this AWS China (aws-cn) account: %w",
		savingsplans.ErrUnsupportedPartition)
}
