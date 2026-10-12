package config

import (
	"encoding/json"
	"fmt"
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
