// Package terraform embeds the shared Terraform modules that ship inside the
// customer federation bundles built by internal/iacbundle (for both the API
// and scripts/generate-federation-iac.go).
package terraform

import "embed"

// ReservationRoleModule holds terraform/modules/iam/azure/cudly-reservation-role,
// which the Azure federation bundle calls. The bundle zips only
// iac/federation/azure-target/terraform, so the module is shipped alongside it
// from here (#1431). Dot files such as the lock file are not embedded.
//
//go:embed modules/iam/azure/cudly-reservation-role/*.tf
var ReservationRoleModule embed.FS

// ReservationRoleModuleDir is the module's directory inside ReservationRoleModule.
const ReservationRoleModuleDir = "modules/iam/azure/cudly-reservation-role"
