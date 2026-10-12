package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ComputeIdentity is the EC2 platform and tenancy of a recommendation, the two
// Details fields that distinguish otherwise identical recommendations (issue
// #806). Values are VERBATIM: AWS platform is the raw Cost Explorer string
// ("Linux/UNIX"), so no trimming or case folding.
type ComputeIdentity struct {
	Platform string
	Tenancy  string
}

// IsZero reports whether neither field is known (non-compute recs, legacy rows
// without Details).
func (c ComputeIdentity) IsZero() bool { return c.Platform == "" && c.Tenancy == "" }

// ComputeIdentity reads platform and tenancy from the rec's Details blob. It
// mirrors the generated columns of migration 000111
// (COALESCE(payload->'details'->>'platform', ”)) and is pinned to them by the
// shared vectors in TestComputeIdentity_Vectors: absent Details, JSON null, a
// missing key and a null value all yield "". Only ComputeDetails carries these
// keys, so the result is service-agnostic. config cannot import pkg/common, so
// this decodes just the two keys. A non-string value is an error.
func (r *RecommendationRecord) ComputeIdentity() (ComputeIdentity, error) {
	if len(r.Details) == 0 {
		return ComputeIdentity{}, nil
	}
	var d struct {
		Platform string `json:"platform"`
		Tenancy  string `json:"tenancy"`
	}
	if err := json.Unmarshal(r.Details, &d); err != nil {
		return ComputeIdentity{}, fmt.Errorf("decode platform/tenancy of %s/%s details: %w", r.Region, r.ResourceType, err)
	}
	return ComputeIdentity{Platform: d.Platform, Tenancy: d.Tenancy}, nil
}

// RecIdentityKey is the tuple a price is a function of: the same eleven
// components (the ninth is the zonal EC2 availability zone, "" for every
// other rec, issue #792; the tenth and eleventh are the EC2 platform and
// tenancy read from Details, "" when absent, issue #806) behind
// RecommendationRecord.ID, with a nil
// and an empty CloudAccountID collapsed together (as purchaseConstraintSets
// and the store's account_key both do). Provider and payment are lowercased
// because validatePurchaseRecommendation canonicalises the request side.
func RecIdentityKey(rec *RecommendationRecord) (string, error) {
	ci, err := rec.ComputeIdentity()
	if err != nil {
		return "", err
	}
	account := ""
	if rec.CloudAccountID != nil {
		account = *rec.CloudAccountID
	}
	return strings.Join([]string{
		strings.ToLower(rec.Provider), account, rec.Service, rec.Region, rec.ResourceType,
		rec.Engine, strconv.Itoa(rec.Term), strings.ToLower(rec.Payment),
		rec.AvailabilityZone, ci.Platform, ci.Tenancy,
	}, "\x1f"), nil
}
