package analytics

import (
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// Legacy commitment_type literals written before #339. The savings_snapshots
// CHECK still admits them (migration 000141) so a not-yet-replaced replica can
// keep writing during a rolling deploy; the collector never emits them.
const (
	legacyCommitmentRI          = "RI"
	legacyCommitmentSavingsPlan = "SavingsPlan"
)

// allowedCommitmentTypes mirrors the savings_snapshots valid_commitment_type
// CHECK constraint (migration 000141): the pkg/common vocabulary plus the two
// legacy literals.
var allowedCommitmentTypes = map[string]bool{
	string(common.CommitmentReservedInstance): true,
	string(common.CommitmentSavingsPlan):      true,
	string(common.CommitmentCUD):              true,
	string(common.CommitmentReservedCapacity): true,
	legacyCommitmentRI:                        true,
	legacyCommitmentSavingsPlan:               true,
}

// commitmentTypeFor maps a purchase's (provider, service) to its
// commitment_type in the pkg/common vocabulary. GCP commitments are
// committed-use discounts, any Savings Plans service slug is a savings plan,
// and everything else (AWS and Azure reservations) is a reserved instance.
func commitmentTypeFor(provider, service string) string {
	switch {
	case strings.EqualFold(provider, string(common.ProviderGCP)):
		return string(common.CommitmentCUD)
	case isSavingsPlanService(service):
		return string(common.CommitmentSavingsPlan)
	default:
		return string(common.CommitmentReservedInstance)
	}
}

// isSavingsPlanService recognizes every Savings Plans service slug persisted in
// purchase_history.service: the umbrella "savingsplans", the four per-plan-type
// slugs, and the pre-#85 "savings-plans" spelling.
func isSavingsPlanService(service string) bool {
	s := strings.ToLower(service)
	return common.IsSavingsPlan(common.ServiceType(s)) || s == "savings-plans"
}

// validateCommitmentType rejects a commitment_type that the savings_snapshots
// table CHECK constraint would reject. Extracted so the guard is unit-testable
// directly (the COPY path acquires a real pooled connection that pgxmock can't
// stand in for, so an end-to-end test can only prove the acquire-failure path).
func validateCommitmentType(commitmentType string) error {
	if !allowedCommitmentTypes[commitmentType] {
		return fmt.Errorf("invalid commitment_type %q", commitmentType)
	}
	return nil
}
