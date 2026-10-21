package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Issue #1075: the spec declares OpenAPI 3.0.3, where nullable needs an
// explicit type, $ref siblings are ignored and exclusiveMinimum is a boolean
// paired with minimum.
func TestOpenAPIDialect303Schemas(t *testing.T) {
	type schema struct {
		Ref              string            `yaml:"$ref"`
		Type             string            `yaml:"type"`
		Nullable         bool              `yaml:"nullable"`
		Required         []string          `yaml:"required"`
		Properties       map[string]schema `yaml:"properties"`
		Minimum          *int              `yaml:"minimum"`
		ExclusiveMinimum any               `yaml:"exclusiveMinimum"`
	}
	var spec struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(openapiSpec, &spec))
	schemas := spec.Components.Schemas

	prop := schemas["RecommendationsFreshness"].Properties["last_collection_partial"]
	require.NotEmpty(t, prop.Ref, "3.0.3 ignores $ref siblings, so the property must be a bare $ref")
	partial := schemas[strings.TrimPrefix(prop.Ref, "#/components/schemas/")]

	// null is accepted only if the resolved schema is nullable with a type.
	assert.True(t, partial.Nullable, "null last_collection_partial must validate")
	assert.Equal(t, "object", partial.Type, "nullable requires an explicit type in 3.0.3")
	// {"providers": []} satisfies required + type.
	assert.Equal(t, []string{"providers"}, partial.Required)
	assert.Equal(t, "array", partial.Properties["providers"].Type)

	stepCap := schemas["PlanRequest"].Properties["max_step_commitment"]
	require.NotNil(t, stepCap.Minimum)
	// minimum 0 with exclusiveMinimum true: positive accepted, 0 rejected.
	assert.Equal(t, 0, *stepCap.Minimum)
	assert.Equal(t, true, stepCap.ExclusiveMinimum, "zero must be rejected; exclusiveMinimum must be boolean in 3.0.3")
}
