package api

import (
	"errors"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/schedexpr"
)

// parseCollectionSchedule parses the deployment's recommendation-collection
// schedule for next_collection_at (issue #756). An empty expression means
// scheduled collection is off; an unparsable one is logged once per handler
// and treated the same way, so the API reports "unknown" instead of failing
// or guessing.
func parseCollectionSchedule(expr string) schedexpr.Schedule {
	s, err := schedexpr.Parse(expr)
	if err != nil {
		if !errors.Is(err, schedexpr.ErrEmpty) {
			logging.Warnf("RECOMMENDATION_SCHEDULE %q is not understood (%v); next_collection_at will be null", expr, err)
		}
		return nil
	}
	return s
}

// NextCollectionAt returns the next scheduled collection strictly after
// now, or nil when it is unknown: no schedule, a rate() schedule (whose
// phase the runtime cannot see) or no fire time within the search horizon.
// Exported so the server package can check the schedule is wired through.
func (h *Handler) NextCollectionAt(now time.Time) *time.Time {
	if h.collectionSchedule == nil {
		return nil
	}
	next, ok := h.collectionSchedule.Next(now)
	if !ok {
		return nil
	}
	return &next
}
