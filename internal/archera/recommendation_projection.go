package archera

import (
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrRecommendationIdentity means a stored recommendation cannot be proven to be a supported savings plan.
var ErrRecommendationIdentity = errors.New("stored recommendation identity is unavailable")

const (
	maxDetailsBytes    = 64 << 10
	maxHourlyBytes     = 64
	maxHourlyExponent  = 100
	storedNoUpfront    = "no-upfront"
	storedPartialUpfr  = "partial-upfront"
	storedAllUpfront   = "all-upfront"
	storedTermOneYear  = 1
	storedTermThreeYrs = 3
)

type storedDetails struct {
	OfferingID       string          `json:"offering_id"`
	PlanType         string          `json:"plan_type"`
	Region           string          `json:"region"`
	InstanceFamily   string          `json:"instance_family"`
	HourlyCommitment json.RawMessage `json:"hourly_commitment"`
}

// ProjectRecommendation turns a stored recommendation into the identity the vendor line items are matched against.
func ProjectRecommendation(r *config.RecommendationRecord) (*RecommendationIdentity, error) {
	if r == nil || len(r.Details) > maxDetailsBytes {
		return nil, ErrRecommendationIdentity
	}
	var d storedDetails
	if err := json.Unmarshal(r.Details, &d); err != nil {
		return nil, ErrRecommendationIdentity
	}
	hourly := storedHourlyAmount(d.HourlyCommitment)
	payment, duration := storedPayment(r.Payment), storedDuration(r.Term)
	if hourly == nil || payment == "" || duration == 0 {
		return nil, ErrRecommendationIdentity
	}
	out := &RecommendationIdentity{Provider: r.Provider, Service: r.Service, Payment: payment,
		TopLevelRegion: r.Region, NativeDurationSeconds: duration, OfferingID: d.OfferingID,
		PlanType: d.PlanType, Region: d.Region, InstanceFamily: d.InstanceFamily, HourlyCommitment: hourly}
	if r.SourceAccountID != nil {
		out.SourceAccountID = *r.SourceAccountID
	}
	if r.CloudAccountID != nil {
		out.CloudAccountID = *r.CloudAccountID
	}
	if !validRecommendation(out) {
		return nil, ErrRecommendationIdentity
	}
	return out, nil
}

// Equal reports whether both identities are non-nil and agree on every projected field.
func (r *RecommendationIdentity) Equal(o *RecommendationIdentity) bool {
	if r == nil || o == nil {
		return false
	}
	if (r.HourlyCommitment == nil) != (o.HourlyCommitment == nil) ||
		r.HourlyCommitment != nil && r.HourlyCommitment.Cmp(o.HourlyCommitment) != 0 {
		return false
	}
	a, b := *r, *o
	a.HourlyCommitment, b.HourlyCommitment = nil, nil
	return a == b
}

func storedPayment(stored string) string {
	switch stored {
	case storedNoUpfront:
		return string(insurance.PaymentNoUpfront)
	case storedPartialUpfr:
		return string(insurance.PaymentPartialUpfront)
	case storedAllUpfront:
		return string(insurance.PaymentAllUpfront)
	}
	return ""
}

func storedDuration(term int) int64 {
	switch term {
	case storedTermOneYear:
		return awsSPYearSeconds
	case storedTermThreeYrs:
		return awsSPThreeYearSeconds
	}
	return 0
}

// storedHourlyAmount accepts only a positive JSON number whose exponent is bounded before big.Rat parses it.
// Quoted numbers, null, booleans and containers are not valid big.Rat input, and the raw token is already valid JSON.
func storedHourlyAmount(raw []byte) *big.Rat {
	if len(raw) > maxHourlyBytes {
		return nil
	}
	text := string(raw)
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exp, err := strconv.Atoi(text[i+1:])
		if err != nil || exp < -maxHourlyExponent || exp > maxHourlyExponent {
			return nil
		}
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok || r.Sign() <= 0 {
		return nil
	}
	return r
}
