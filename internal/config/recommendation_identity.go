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

// SuppressionVariant is the rec's availability zone, platform and tenancy as
// the purchase suppression key carries them (issue #816): the platform and
// tenancy come from the same Details read (ComputeIdentity) as RecIdentityKey,
// so the write side (BuildSuppressions) and the read side (the scheduler's
// subtraction pass) cannot drift from it or from each other. For AWS EC2 only,
// legacy tenancy "shared" is canonicalised to "default" and an AZ stored only
// in Details is used when the AZ field is empty (issue #1232).
func (r *RecommendationRecord) SuppressionVariant() (SuppressionVariant, error) {
	ci, err := r.ComputeIdentity()
	if err != nil {
		return SuppressionVariant{}, err
	}
	v := SuppressionVariant{AvailabilityZone: r.AvailabilityZone, Platform: ci.Platform, Tenancy: ci.Tenancy}
	if isAWSEC2(r.Provider, r.Service) {
		v.Tenancy = canonicalSuppressionTenancy(v.Tenancy)
		if v.AvailabilityZone == "" {
			// Rows saved 2026-10-09..12 hold the zone only in Details (#1232).
			v.AvailabilityZone = r.detailsAvailabilityZone()
		}
	}
	return v, nil
}

// isAWSEC2 is the scope of the legacy-shape canonicalisation: only AWS EC2
// ever stored tenancy "shared" or the zone only in Details.
func isAWSEC2(provider, service string) bool {
	return provider == "aws" && CanonicalSuppressionService(provider, service) == "compute"
}

// canonicalSuppressionTenancy maps the pre-2026-05-20 EC2 tenancy spelling
// "shared" to today's "default" so old and new suppressions and recs match.
// common.NormalizeTenancyName is not used: it also case-folds and maps every
// unknown value to "", which would make distinct unknown tenancies match.
func canonicalSuppressionTenancy(t string) string {
	if strings.EqualFold(strings.TrimSpace(t), "shared") {
		return "default"
	}
	return t
}

// detailsAvailabilityZone reads the zonal AZ from Details; "" when absent or
// unreadable, trimmed like the other Details-AZ readers. A non-string zone is
// treated as absent (regional), which can under-suppress; no producer writes one.
func (r *RecommendationRecord) detailsAvailabilityZone() string {
	var d struct {
		AvailabilityZone string `json:"availability_zone"`
	}
	if json.Unmarshal(r.Details, &d) != nil {
		return ""
	}
	return strings.TrimSpace(d.AvailabilityZone)
}
