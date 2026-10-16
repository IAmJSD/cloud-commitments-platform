package archera

import "math/big"

type CandidateIdentity struct {
	OfferID, Provider, Type, Payment string
	SelectedAmount                   *big.Rat
}

type CandidateAssociation struct {
	OfferID, Status, Reason, NativeIdentityStatus string
}

func associateCandidate(r *RecommendationIdentity, c *CandidateIdentity) CandidateAssociation {
	out := CandidateAssociation{OfferID: c.OfferID, Status: "unavailable", Reason: "candidate_basis_unproven", NativeIdentityStatus: "unavailable"}
	if validMatchUUID(c.OfferID) && c.Provider == r.Provider && c.Type == "savings_plan" && validPayment(c.Payment) && sameAmount(r.HourlyCommitment, c.SelectedAmount) {
		out.Status, out.Reason = "vendor_alternative", ""
	}
	return out
}
