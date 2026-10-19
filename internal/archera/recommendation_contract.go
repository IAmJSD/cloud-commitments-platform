package archera

import (
	"errors"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

// ProjectContract projects the published comparison and line items into the identities MatchRecommendation joins.
func ProjectContract(c *insurance.Comparison, items []insurance.LineItem) ([]ComparisonIdentity, []LineItemIdentity, error) {
	if c == nil {
		return nil, nil, errors.New("archera comparison identity is unavailable")
	}
	rows := make([]ComparisonIdentity, 0, len(c.Rows))
	for i := range c.Rows {
		r := &c.Rows[i]
		current := contractCandidate(&r.Current)
		rows = append(rows, ComparisonIdentity{Region: r.Current.Region, IsCurrent: r.Current.IsCurrent,
			LineItemID: r.LineItemID, CurrentOfferID: current.OfferID, Provider: current.Provider,
			CurrentType: current.Type, CurrentPayment: current.Payment, CurrentSelectedAmount: current.SelectedAmount,
			Candidates: vendorAlternatives(r.Candidates)})
	}
	identities := make([]LineItemIdentity, 0, len(items))
	for i := range items {
		identities = append(identities, contractLineItem(&items[i]))
	}
	return rows, identities, nil
}

// vendorEntries drops the entry the vendor flags as the current offer, which is never an alternative.
func vendorEntries(entries []insurance.OfferEntry) []*insurance.OfferEntry {
	out := make([]*insurance.OfferEntry, 0, len(entries))
	for i := range entries {
		if !entries[i].IsCurrent {
			out = append(out, &entries[i])
		}
	}
	return out
}

func vendorAlternatives(entries []insurance.OfferEntry) []CandidateIdentity {
	vendor := vendorEntries(entries)
	out := make([]CandidateIdentity, 0, len(vendor))
	for _, e := range vendor {
		out = append(out, contractCandidate(e))
	}
	return out
}

func contractLineItem(li *insurance.LineItem) LineItemIdentity {
	offer := &li.Offer
	kind := offer.Type
	if !li.IsSelected || (offer.PaymentOption != nil && *offer.PaymentOption != li.PaymentOption) {
		kind = ""
	}
	return LineItemIdentity{
		ID: li.ID, PlanID: li.PlanID, OfferID: li.OfferID, Provider: string(offer.Provider),
		SourceAccountID: identityText(li.AccountID), ProviderOfferingID: offer.ProviderOfferingID, Type: kind,
		PlanType: identityText(offer.PlanType), Region: identityText(offer.Region), InstanceFamily: identityText(offer.InstanceFamily),
		Payment: string(li.PaymentOption), DurationSeconds: offer.DurationSeconds, SelectedCommitment: li.SelectedCommitment,
	}
}

func contractCandidate(offer *insurance.OfferEntry) CandidateIdentity {
	out := CandidateIdentity{OfferID: offer.OfferID, Provider: string(offer.Provider),
		Type: offer.CommitmentType, SelectedAmount: offer.SelectedAmount}
	if offer.PaymentOption != nil {
		out.Payment = string(*offer.PaymentOption)
	}
	return out
}

func identityText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
