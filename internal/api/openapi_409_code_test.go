package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestOpenAPI409NamesGuardCode keeps the contract honest: every legacy
// operation that can answer a managed-plan refusal documents a 409 whose
// schema carries the machine-readable code.
func TestOpenAPI409NamesGuardCode(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `yaml:"$ref"`
					} `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"responses"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))

	for _, op := range []struct{ path, method string }{
		{"/api/plans/{id}", "put"}, {"/api/plans/{id}", "patch"}, {"/api/plans/{id}", "delete"},
		{"/api/plans/{id}/purchases", "post"}, {"/api/purchases/planned/{id}/resume", "post"},
	} {
		node := doc.Paths[op.path][op.method]
		var operation struct {
			Responses map[string]struct {
				Ref     string `yaml:"$ref"`
				Content map[string]struct {
					Schema struct {
						Ref string `yaml:"$ref"`
					} `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"responses"`
		}
		require.NoError(t, node.Decode(&operation), "%s %s", op.method, op.path)
		r, ok := operation.Responses["409"]
		require.True(t, ok, "%s %s must document 409", op.method, op.path)
		schema := r.Content["application/json"].Schema.Ref
		if r.Ref != "" {
			schema = doc.Components.Responses[filepath.Base(r.Ref)].Content["application/json"].Schema.Ref
		}
		require.Equal(t, "#/components/schemas/ErrorWithCode", schema, "%s %s", op.method, op.path)
	}
}
