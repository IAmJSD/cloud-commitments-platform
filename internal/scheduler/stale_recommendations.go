package scheduler

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// staleRecommendationRetentionEnvVar overrides
// config.DefaultStaleRecommendationRetentionDays: the number of days a
// (provider, account) pair may go without collecting before its cached
// recommendations are deleted (issue #120).
const staleRecommendationRetentionEnvVar = "CUDLY_STALE_RECOMMENDATION_RETENTION_DAYS"

// staleRecommendationRetentionDaysFromEnv reads
// CUDLY_STALE_RECOMMENDATION_RETENTION_DAYS on every call, so a changed value
// takes effect on the next collection without a restart. Unset uses the
// default; a value that is not a positive whole number of days logs a warning
// and uses the default too, so a typo can never turn into "delete everything"
// (0) or an error that skips the prune for good.
func staleRecommendationRetentionDaysFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(staleRecommendationRetentionEnvVar))
	if raw == "" {
		return config.DefaultStaleRecommendationRetentionDays
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days <= 0 {
		logging.Warnf("%s invalid (%q, want a positive number of days); using default %d",
			staleRecommendationRetentionEnvVar, raw, config.DefaultStaleRecommendationRetentionDays)
		return config.DefaultStaleRecommendationRetentionDays
	}
	return days
}

// pruneStaleRecommendationsBestEffort deletes the cached rows of the
// (provider, account) pairs that have not collected within the retention
// window. It runs after every persisted collection, which is daily on every
// cloud by default, rather than in the cleanup sweep, which is opt-in on every
// platform. A failure is logged and never fails the collection: the rows it
// would have deleted are simply kept until the next run.
func (s *Scheduler) pruneStaleRecommendationsBestEffort(ctx context.Context) {
	days := staleRecommendationRetentionDaysFromEnv()
	deleted, err := s.config.PruneStaleRecommendations(ctx, days)
	if err != nil {
		logging.Errorf("Failed to prune recommendations of pairs not collected for %d days: %v", days, err)
		return
	}
	if deleted > 0 {
		logging.Infof("Pruned %d recommendations of (provider, account) pairs not collected for %d days", deleted, days)
	}
}
