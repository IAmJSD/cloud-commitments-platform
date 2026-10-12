package config

// TotalCommitment returns the recommendation's full committed spend: the
// upfront cost plus the recurring monthly cost over the term. UpfrontCost and
// MonthlyCost are totals for the rec's Count, so they sum with no per-instance
// scaling. Term is in years (converted with *12). A nil MonthlyCost (all-upfront
// commitments) or a non-positive Term contributes nothing to the recurring leg.
// Used for spend caps instead of UpfrontCost alone, which is $0 for no-upfront
// commitments (Azure monthly billing).
func (r *RecommendationRecord) TotalCommitment() float64 {
	total := r.UpfrontCost
	if r.Term > 0 && r.MonthlyCost != nil {
		total += *r.MonthlyCost * float64(r.Term*12)
	}
	return total
}
