package archera

import (
	"errors"
	"math/big"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const (
	awsSPYearSeconds      int64 = 31536000
	awsSPThreeYearSeconds int64 = 94608000
)

type RecommendationIdentity struct {
	Provider, SourceAccountID, Service, PlanType, OfferingID string
	Region, TopLevelRegion, InstanceFamily, Payment          string
	NativeDurationSeconds                                    int64
	HourlyCommitment                                         *big.Rat
}

type LineItemIdentity struct {
	ID, PlanID, OfferID, Provider, SourceAccountID, ProviderOfferingID string
	Type, PlanType, Region, InstanceFamily, Payment                    string
	DurationSeconds, SelectedCommitment                                *big.Rat
}

type ComparisonIdentity struct {
	Region                                                            *string
	IsCurrent                                                         bool
	LineItemID, CurrentOfferID, Provider, CurrentType, CurrentPayment string
	CurrentSelectedAmount                                             *big.Rat
}

type MatchStatus string

const (
	MatchUnavailable   MatchStatus = "unavailable"
	ResourceAssociated MatchStatus = "resource_associated"
)

type RecommendationMatch struct {
	Status                                               MatchStatus
	LineItemID, Reason, AmountProvenance, EconomicStatus string
}

func MatchRecommendation(planID string, rec *RecommendationIdentity, rows []ComparisonIdentity, items []LineItemIdentity) (RecommendationMatch, error) {
	unavailable := RecommendationMatch{Status: MatchUnavailable, Reason: "identity_unproven"}
	if !validRecommendation(rec) {
		return unavailable, nil
	}
	index, err := comparisonIndex(planID, rows, items)
	if err != nil {
		return unavailable, err
	}
	if index == nil {
		return unavailable, nil
	}
	var selected *LineItemIdentity
	for i := range items {
		if !sameResource(rec, &items[i]) {
			continue
		}
		if selected != nil {
			unavailable.Reason = "ambiguous_line_items"
			return unavailable, nil
		}
		selected = &items[i]
	}
	if selected == nil {
		return unavailable, nil
	}
	row := index[strings.ToLower(selected.ID)]
	if !sameCurrentBasis(rec, row) {
		unavailable.Reason = "comparison_basis_unproven"
		return unavailable, nil
	}
	result := RecommendationMatch{
		Status: ResourceAssociated, LineItemID: row.LineItemID,
		AmountProvenance: "stored_decimal_only", EconomicStatus: "unavailable_source_precision",
	}
	return result, nil
}

func validRecommendation(r *RecommendationIdentity) bool {
	if r == nil {
		return false
	}
	if r.Provider != "aws" || r.Service != "savings-plans-ec2instance" || r.PlanType != "EC2Instance" {
		return false
	}
	if r.NativeDurationSeconds != awsSPYearSeconds && r.NativeDurationSeconds != awsSPThreeYearSeconds {
		return false
	}
	return validDimensions(r) && validPayment(r.Payment) && sameAmount(r.HourlyCommitment, r.HourlyCommitment)
}

func validDimensions(r *RecommendationIdentity) bool {
	for _, value := range []string{r.SourceAccountID, r.OfferingID, r.Region, r.InstanceFamily} {
		if value == "" {
			return false
		}
	}
	return usableSource(r.SourceAccountID) && (r.TopLevelRegion == "" || r.TopLevelRegion == r.Region)
}

func validPayment(payment string) bool {
	switch payment {
	case "no_upfront", "partial_upfront", "all_upfront":
		return true
	default:
		return false
	}
}

func sameAmount(a, b *big.Rat) bool {
	return a != nil && b != nil && a.Sign() > 0 && b.Sign() > 0 && a.Cmp(b) == 0
}

type resourceIdentity struct {
	provider, source, offering, plan, region, family, payment string
}

func sameResource(r *RecommendationIdentity, item *LineItemIdentity) bool {
	want := resourceIdentity{r.Provider, r.SourceAccountID, r.OfferingID, r.PlanType, r.Region, r.InstanceFamily, r.Payment}
	got := resourceIdentity{item.Provider, item.SourceAccountID, item.ProviderOfferingID, item.PlanType, item.Region, item.InstanceFamily, item.Payment}
	return want == got && item.Type == "savings_plan" && item.DurationSeconds != nil &&
		item.DurationSeconds.Cmp(new(big.Rat).SetInt64(r.NativeDurationSeconds)) == 0 &&
		sameAmount(r.HourlyCommitment, item.SelectedCommitment)
}

func sameCurrentBasis(r *RecommendationIdentity, row *ComparisonIdentity) bool {
	return row != nil && row.IsCurrent && row.Region != nil && *row.Region == r.Region && row.Provider == r.Provider && row.CurrentType == "savings_plan" &&
		row.CurrentPayment == r.Payment && sameAmount(r.HourlyCommitment, row.CurrentSelectedAmount)
}

func comparisonIndex(planID string, rows []ComparisonIdentity, items []LineItemIdentity) (map[string]*ComparisonIdentity, error) {
	if !validMatchUUID(planID) {
		return nil, nil
	}
	lineItems := make(map[string]*LineItemIdentity, len(items))
	for i := range items {
		if !validLineReference(planID, &items[i]) {
			return nil, nil
		}
		id := strings.ToLower(items[i].ID)
		if lineItems[id] != nil {
			return nil, errors.New("duplicate Archera line-item reference")
		}
		lineItems[id] = &items[i]
	}
	index := make(map[string]*ComparisonIdentity, len(rows))
	for i := range rows {
		if !validComparisonReference(&rows[i], lineItems) {
			return nil, nil
		}
		id := strings.ToLower(rows[i].LineItemID)
		if index[id] != nil {
			return nil, errors.New("duplicate Archera comparison reference")
		}
		index[id] = &rows[i]
	}
	return index, nil
}

func usableSource(source string) bool {
	for _, r := range source {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func validMatchUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func validLineReference(planID string, item *LineItemIdentity) bool {
	return validMatchUUID(item.ID) && validMatchUUID(item.OfferID) && validMatchUUID(item.PlanID) && strings.EqualFold(planID, item.PlanID)
}

func validComparisonReference(row *ComparisonIdentity, items map[string]*LineItemIdentity) bool {
	if !validMatchUUID(row.LineItemID) || !validMatchUUID(row.CurrentOfferID) {
		return false
	}
	item := items[strings.ToLower(row.LineItemID)]
	return item != nil && strings.EqualFold(row.CurrentOfferID, item.OfferID)
}
