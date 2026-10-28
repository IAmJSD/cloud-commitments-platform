// Package ricost holds the convertible Reserved Instance cost formula shared
// by the reshape recommendations API (internal/api) and the auto-exchange and
// ladder paths (internal/server), so the two can't drift apart.
package ricost

import (
	"errors"
	"fmt"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
)

// HoursPerMonth is AWS's canonical hours-per-month convention for monthly
// cost figures: 8760 hours in a 365-day year divided by 12 months. It matches
// hoursPerMonth in cloud-commitments-go's providers/aws/recommendations and
// the 730-hour monthly rates used elsewhere in the platform.
const HoursPerMonth = 730.0

// secondsPerHour converts an RI's Duration (seconds) into hours.
const secondsPerHour = 3600

// Sentinel causes for MonthlyCostFromConvertibleRI's error, so the reshape
// API can show each excluded RI's reason.
var (
	ErrNonPositiveDuration = errors.New("non-positive duration")
	ErrNoPricing           = errors.New("no positive price")
)

// MonthlyCostFromConvertibleRI computes the per-instance per-month effective
// cost from an RI's pricing fields, matching the formula the AWS
// effectiveMonthlyCost function uses for offerings:
//
//	monthly = (FixedPrice / hours_per_term + UsagePrice + recurring_hourly) × HoursPerMonth
//
// Returns an error when Duration is non-positive or when the computed cost is
// not positive (for example FixedPrice, UsagePrice and RecurringHourlyAmount
// all zero, which means AWS returned no pricing, not a free RI, issue #1188):
// a zero cost would read as a free RI and skip reshape's dollar-units pricing
// gate, so callers must exclude the RI. The error wraps ErrNonPositiveDuration
// or ErrNoPricing.
//
// Used to populate exchange.RIInfo.MonthlyCost so the cross-family
// dollar-units pre-filter compares against per-target offering costs computed
// with the same formula.
func MonthlyCostFromConvertibleRI(ri ec2svc.ConvertibleRI) (float64, error) {
	if ri.Duration <= 0 {
		return 0, fmt.Errorf("convertible RI %s has %w %d: monthly cost is not computable", ri.ReservedInstanceID, ErrNonPositiveDuration, ri.Duration)
	}
	hoursPerTerm := float64(ri.Duration) / secondsPerHour
	cost := ((ri.FixedPrice / hoursPerTerm) + ri.UsagePrice + ri.RecurringHourlyAmount) * HoursPerMonth
	// !(cost > 0) also rejects NaN, which is no usable price either.
	if !(cost > 0) {
		return 0, fmt.Errorf("convertible RI %s has %w (fixed %v, usage %v, recurring hourly %v): monthly cost is not computable",
			ri.ReservedInstanceID, ErrNoPricing, ri.FixedPrice, ri.UsagePrice, ri.RecurringHourlyAmount)
	}
	return cost, nil
}
