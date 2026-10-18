package archera

import (
	"errors"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

// errRecommendationRowMissing is returned when an associated match has no vendor row to render.
var errRecommendationRowMissing = errors.New("archera recommendation row is unavailable")

// RecommendationComparisonDTO is the recommendation-only response: one current offer and its
// vendor-linked candidates. It carries no plan totals, deltas, guarantees, source account or org ID.
type RecommendationComparisonDTO struct {
	RecommendationID      string                         `json:"recommendation_id"`
	Status                MatchStatus                    `json:"status"`
	Reason                Reason                         `json:"reason"`
	AmountProvenance      Reason                         `json:"amount_provenance"`
	EconomicStatus        Reason                         `json:"economic_status"`
	FetchedAt             string                         `json:"fetched_at"`
	Currency              *string                        `json:"currency"`
	CurrencyNote          string                         `json:"currency_note"`
	ComparableDelta       *string                        `json:"comparable_delta"`
	PremiumIncluded       bool                           `json:"premium_included"`
	Current               *RecommendationOfferDTO        `json:"current"`
	Candidates            []RecommendationAlternativeDTO `json:"candidates"`
	NonGatingDisclosure   string                         `json:"non_gating_disclosure"`
	SponsorshipDisclosure string                         `json:"sponsorship_disclosure"`
}

// RecommendationOfferDTO names the vendor's units explicitly. The native_* fields come from the
// line item and stay null on candidates, which the vendor does not give a native identity.
type RecommendationOfferDTO struct {
	OfferID                  string        `json:"offer_id"`
	Provider                 string        `json:"provider"`
	CommitmentType           string        `json:"commitment_type"`
	HourlySelectedCommitment *string       `json:"hourly_selected_commitment"`
	NativeDurationSeconds    *string       `json:"native_duration_seconds"`
	NativePlanType           *string       `json:"native_plan_type"`
	NativeInstanceFamily     *string       `json:"native_instance_family"`
	EffectiveContractTerm    *string       `json:"effective_contract_term"`
	PaymentOption            *string       `json:"payment_option"`
	LeaseAttached            bool          `json:"lease_attached"`
	Monthly                  FinancialsDTO `json:"monthly"`
	UpfrontCost              *string       `json:"upfront_one_time_cost"`
}

type RecommendationAlternativeDTO struct {
	OfferID              string                  `json:"offer_id"`
	Status               CandidateStatus         `json:"status"`
	Reason               Reason                  `json:"reason"`
	NativeIdentityStatus CandidateStatus         `json:"native_identity_status"`
	Figures              *RecommendationOfferDTO `json:"figures"`
}

// BuildRecommendationComparison matches the stored recommendation against the vendor line items
// and renders only the matched row. An unmatched result carries no figures. It returns
// ErrUnrepresentable when a rendered number has no exact decimal form.
func BuildRecommendationComparison(id, planID string, rec *RecommendationIdentity, c *insurance.Comparison, items []insurance.LineItem) (*RecommendationComparisonDTO, error) {
	rows, identities, err := ProjectContract(c, items)
	if err != nil {
		return nil, err
	}
	match, err := MatchRecommendation(planID, rec, rows, identities)
	if err != nil {
		return nil, err
	}
	out := &RecommendationComparisonDTO{
		RecommendationID: id, Status: match.Status, Reason: match.Reason,
		AmountProvenance: match.AmountProvenance, EconomicStatus: ReasonIdentityUnproven,
		FetchedAt: c.FetchedAt.UTC().Format(time.RFC3339), CurrencyNote: currencyNote,
		PremiumIncluded: true, Candidates: []RecommendationAlternativeDTO{},
		NonGatingDisclosure: common.ArcheraNonGatingDisclosure, SponsorshipDisclosure: common.ArcheraSponsorshipDisclosure,
	}
	if match.Status != ResourceAssociated {
		return out, nil
	}
	out.EconomicStatus = match.EconomicStatus
	m := &mapper{}
	if err := m.fillMatched(out, &match, c, items); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *mapper) fillMatched(out *RecommendationComparisonDTO, match *RecommendationMatch, c *insurance.Comparison, items []insurance.LineItem) error {
	row := findRow(c.Rows, match.LineItemID)
	if row == nil {
		return errRecommendationRowMissing
	}
	out.Current = m.recommendationOffer(&row.Current)
	out.Candidates = m.recommendationAlternatives(vendorEntries(row.Candidates), match.Candidates)
	for i := range items {
		if strings.EqualFold(items[i].ID, match.LineItemID) {
			offer := &items[i].Offer
			out.Current.NativeDurationSeconds = m.dec(offer.DurationSeconds)
			out.Current.NativePlanType = cleanPtr(offer.PlanType)
			out.Current.NativeInstanceFamily = cleanPtr(offer.InstanceFamily)
		}
	}
	return m.err
}

func findRow(rows []insurance.ComparisonRow, lineItemID string) *insurance.ComparisonRow {
	for i := range rows {
		if strings.EqualFold(rows[i].LineItemID, lineItemID) {
			return &rows[i]
		}
	}
	return nil
}

func (m *mapper) recommendationOffer(e *insurance.OfferEntry) *RecommendationOfferDTO {
	return &RecommendationOfferDTO{
		OfferID: CleanString(e.OfferID), Provider: CleanString(string(e.Provider)), CommitmentType: CleanString(e.CommitmentType),
		HourlySelectedCommitment: m.dec(e.SelectedAmount), EffectiveContractTerm: cleanPtr(e.ContractTerm),
		PaymentOption: payment(e.PaymentOption), LeaseAttached: e.LeaseBacked(),
		Monthly: m.financials(e.Monthly), UpfrontCost: m.dec(e.UpfrontCost),
	}
}

// recommendationAlternatives pairs the vendor alternatives with their associations by index.
func (m *mapper) recommendationAlternatives(entries []*insurance.OfferEntry, associations []CandidateAssociation) []RecommendationAlternativeDTO {
	out := make([]RecommendationAlternativeDTO, 0, len(associations))
	for i := range associations {
		a := &associations[i]
		alt := RecommendationAlternativeDTO{OfferID: CleanString(a.OfferID), Status: a.Status,
			Reason: a.Reason, NativeIdentityStatus: a.NativeIdentityStatus}
		if a.Status == CandidateVendorAlternative {
			alt.Figures = m.recommendationOffer(entries[i])
		}
		out = append(out, alt)
	}
	return out
}
