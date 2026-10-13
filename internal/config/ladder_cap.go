package config

import (
	"fmt"
	"strconv"
	"strings"
)

// MinLadderHourlyCommit and MaxLadderHourlyCommitLimit bound
// ladder_configs.max_hourly_commit_per_run. Below one micro-dollar the
// NUMERIC(20,6) column rounds to 0; the upper bound keeps the micro-dollar
// value inside int64 and far above any real hourly commitment.
const (
	MinLadderHourlyCommit      = 0.000001
	MaxLadderHourlyCommitLimit = 1e12
)

// LadderCapMicros returns the per-run hourly cap in exact micro-dollars. Caps
// with more than 6 decimals are rejected instead of rounded, so the stored and
// the compared value are identical (same approach as StepCapCents).
func LadderCapMicros(v float64) (int64, error) {
	if !(v >= MinLadderHourlyCommit) || !(v <= MaxLadderHourlyCommitLimit) {
		return 0, fmt.Errorf("max_hourly_commit_per_run must be between %v and %v, got: %v", MinLadderHourlyCommit, MaxLadderHourlyCommitLimit, v)
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(v, 'f', -1, 64), ".")
	if len(frac) > 6 {
		return 0, fmt.Errorf("max_hourly_commit_per_run must have at most 6 decimal places, got: %v", v)
	}
	n, err := strconv.ParseInt(whole+(frac + "000000")[:6], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("max_hourly_commit_per_run is not representable: %w", err)
	}
	return n, nil
}
