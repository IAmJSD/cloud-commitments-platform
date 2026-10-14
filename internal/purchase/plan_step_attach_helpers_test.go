package purchase

import (
	"context"
	"sync/atomic"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
)

// fakeResolver stands in for planstep.Resolve (fixture): it returns recs or
// err and counts calls; before, when set, runs after the call is counted.
type fakeResolver struct {
	recs   []config.RecommendationRecord
	err    error
	calls  atomic.Int32
	before func()
}

func (f *fakeResolver) Resolve(context.Context, *config.PurchasePlan, []string, int, config.PlanRampBaseline, map[string]config.PlanRampQuantity) (planstep.Result, error) {
	f.calls.Add(1)
	if f.before != nil {
		f.before()
	}
	return planstep.Result{Recs: f.recs}, f.err
}
