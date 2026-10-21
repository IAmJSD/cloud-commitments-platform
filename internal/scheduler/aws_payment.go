package scheduler

import (
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// awsRecommendationPayment maps the cross-provider global default payment to a
// concrete AWS option. It never returns "": the Savings Plans recommendation
// call rejects an empty payment option (#1229). "" maps to the app's own
// default silently; Azure/GCP "upfront" and "monthly" map by schedule
// (config.PaymentScheduleFor) and anything unknown falls back to no-upfront,
// each with one WARN naming the stored value.
func awsRecommendationPayment(stored string) string {
	if stored == "" {
		return "no-upfront"
	}
	if canon, ok := config.NormalizePaymentOption("aws", stored); ok {
		return canon
	}
	mapped := "no-upfront"
	if config.PaymentScheduleFor(stored) == config.PaymentScheduleUpfront {
		mapped = "all-upfront"
	}
	logging.Warnf("global default_payment %q is not an AWS payment option; using %q for AWS recommendations", stored, mapped)
	return mapped
}
