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
