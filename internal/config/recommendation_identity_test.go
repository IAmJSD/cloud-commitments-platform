package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vectors mirror TestMigration_RecommendationsPlatformTenancy (migration
// 000111, internal/database/postgres/migrations) so the Go reader and the
// generated columns cannot diverge. Details here is the details blob itself.
func TestComputeIdentity_Vectors(t *testing.T) {
	vectors := []struct {
		name, details, platform, tenancy string
	}{
		{"details-absent", ``, "", ""},
		{"details-null", `null`, "", ""},
		{"details-empty", `{}`, "", ""},
		{"platform-null", `{"platform":null,"tenancy":null}`, "", ""},
		{"aws-linux", `{"platform":"Linux/UNIX","tenancy":"default"}`, "Linux/UNIX", "default"},
		{"aws-rhel", `{"platform":"Red Hat Enterprise Linux","tenancy":"dedicated"}`, "Red Hat Enterprise Linux", "dedicated"},
		{"verbatim-spaces", `{"platform":" x ","tenancy":" Y "}`, " x ", " Y "},
		{"empty-strings", `{"platform":"","tenancy":""}`, "", ""},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			rec := RecommendationRecord{Details: json.RawMessage(v.details)}
			got, err := rec.ComputeIdentity()
			require.NoError(t, err)
			assert.Equal(t, ComputeIdentity{Platform: v.platform, Tenancy: v.tenancy}, got)
		})
	}
}

func TestComputeIdentity_TopLevelKeyIgnoredAndMalformedFailsLoud(t *testing.T) {
	// Only Details carries the fields (matches the "top-level-ignored" PG vector).
	rec := RecommendationRecord{Details: json.RawMessage(`{"instance_type":"m5.large"}`)}
	got, err := rec.ComputeIdentity()
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	bad := RecommendationRecord{Details: json.RawMessage(`{"platform":5}`)}
	_, err = bad.ComputeIdentity()
	assert.Error(t, err)
}

// The vectors mirror TestMigration_RecommendationsAZConfig (migration 000154,
// internal/database/postgres/migrations) so the Go reader and the generated
// az_config column cannot diverge (issue #408). Details here is the details
// blob itself.
func TestDetailsAZConfig_Vectors(t *testing.T) {
	vectors := []struct {
		name, details, azConfig string
	}{
		{"details-absent", ``, ""},
		{"details-null", `null`, ""},
		{"details-empty", `{}`, ""},
		{"az-config-null", `{"az_config":null}`, ""},
		{"empty-string", `{"az_config":""}`, ""},
		{"rds-single-az", `{"engine":"mysql","az_config":"single-az","instance_class":"db.r5.large"}`, "single-az"},
		{"rds-multi-az", `{"engine":"mysql","az_config":"multi-az","instance_class":"db.r5.large"}`, "multi-az"},
		{"azure-zone-redundant", `{"engine":"sqlserver","az_config":"zoneRedundant"}`, "zoneRedundant"},
		{"verbatim-spaces", `{"az_config":" Multi-AZ "}`, " Multi-AZ "},
		{"compute-details", `{"platform":"Linux/UNIX","tenancy":"default","scope":"Region"}`, ""},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			rec := RecommendationRecord{Details: json.RawMessage(v.details)}
			got, err := rec.DetailsAZConfig()
			require.NoError(t, err)
			assert.Equal(t, v.azConfig, got)
		})
	}
}

func TestDetailsAZConfig_MalformedFailsLoud(t *testing.T) {
	bad := RecommendationRecord{Details: json.RawMessage(`{"az_config":5}`)}
	_, err := bad.DetailsAZConfig()
	require.Error(t, err)
	_, err = RecIdentityKey(&bad)
	require.Error(t, err, "the identity key must not silently drop an unreadable az_config")
}

// Two otherwise identical RDS rows that differ only by az_config have
// different identity keys; the same az_config yields the same key.
func TestRecIdentityKey_DiffersByAZConfig(t *testing.T) {
	rds := func(azConfig string) *RecommendationRecord {
		return &RecommendationRecord{
			Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.r5.large", Engine: "mysql",
			Term: 1, Payment: "all-upfront", Details: json.RawMessage(`{"engine":"mysql","az_config":"` + azConfig + `"}`),
		}
	}
	key := func(r *RecommendationRecord) string {
		k, err := RecIdentityKey(r)
		require.NoError(t, err)
		return k
	}
	assert.NotEqual(t, key(rds("single-az")), key(rds("multi-az")))
	assert.NotEqual(t, key(rds("single-az")), key(rds("")))
	assert.Equal(t, key(rds("multi-az")), key(rds("multi-az")))
}
