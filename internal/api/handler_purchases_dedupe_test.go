package api

import (
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

// TestMatchDuplicateInList pins each clause of the Go-side duplicate check
// that persistExecutionAndSuppressions runs over ListRecentSubmitsTx's rows:
// web source, window start (inclusive), creator (nil creator equals ""),
// and the recomputed idempotency key (which also hashes the creator, so the
// creator cases hold through either check). The SQL predicate applies the same
// source, window and creator filters; this keeps the Go side honest if the
// rows it is given ever widen (#266).
func TestMatchDuplicateInList(t *testing.T) {
	t.Parallel()
	now := time.Now()
	creator := "11111111-1111-1111-1111-111111111111"
	other := "22222222-2222-2222-2222-222222222222"
	recs := []config.RecommendationRecord{validRec()}
	key := mustPurchaseIdempotencyKey(creator, recs, 100)
	exec := func(mut func(e *config.PurchaseExecution)) config.PurchaseExecution {
		e := config.PurchaseExecution{
			ExecutionID: "cand", Status: "pending", Source: common.PurchaseSourceWeb,
			ScheduledDate: now.Add(-30 * time.Second), CreatedByUserID: &creator,
			Recommendations: append([]config.RecommendationRecord(nil), recs...), CapacityPercent: 100,
		}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	for _, tc := range []struct {
		name    string
		cand    config.PurchaseExecution
		creator string
		key     string
		wantDup bool
	}{
		{"exact match", exec(nil), creator, key, true},
		{"window start is inclusive", exec(func(e *config.PurchaseExecution) { e.ScheduledDate = now.Add(-purchaseIdempotencyWindow) }), creator, key, true},
		{"before the window", exec(func(e *config.PurchaseExecution) {
			e.ScheduledDate = now.Add(-purchaseIdempotencyWindow - time.Second)
		}), creator, key, false},
		{"not web-submitted", exec(func(e *config.PurchaseExecution) { e.Source = common.PurchaseSourceCLI }), creator, key, false},
		{"other creator", exec(func(e *config.PurchaseExecution) { e.CreatedByUserID = &other }), creator, key, false},
		{"no creator vs a creator", exec(func(e *config.PurchaseExecution) { e.CreatedByUserID = nil }), creator, key, false},
		{"no creator on both sides", exec(func(e *config.PurchaseExecution) { e.CreatedByUserID = nil }), "", mustPurchaseIdempotencyKey("", recs, 100), true},
		{"different capacity", exec(func(e *config.PurchaseExecution) { e.CapacityPercent = 50 }), creator, key, false},
		{"different rec set", exec(func(e *config.PurchaseExecution) { e.Recommendations[0].Count++ }), creator, key, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := matchDuplicateInList([]config.PurchaseExecution{tc.cand}, tc.creator, tc.key, now)
			if tc.wantDup {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}
