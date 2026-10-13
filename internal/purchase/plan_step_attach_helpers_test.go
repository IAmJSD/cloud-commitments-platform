package purchase

import (
	"context"
	"sync/atomic"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// fakeResolver stands in for planstep.Resolve (fixture): it returns recs or
// err and counts calls; before, when set, runs after the call is counted.
type fakeResolver struct {
	recs   []config.RecommendationRecord
	err    error
	calls  atomic.Int32
	before func()
}

func (f *fakeResolver) Resolve(context.Context, *config.PurchasePlan, []string) ([]config.RecommendationRecord, error) {
	f.calls.Add(1)
	if f.before != nil {
		f.before()
	}
	return f.recs, f.err
}
