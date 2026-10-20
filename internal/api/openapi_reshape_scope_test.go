package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// reshapeSpecOp is the slice of an openapi.yaml operation these checks read.
type reshapeSpecOp struct {
	Parameters []struct {
		Ref      string `yaml:"$ref"`
		Name     string `yaml:"name"`
		Required bool   `yaml:"required"`
	} `yaml:"parameters"`
	Responses map[string]struct {
		Ref         string `yaml:"$ref"`
		Description string `yaml:"description"`
	} `yaml:"responses"`
}

func loadReshapeSpecOp(t *testing.T, path, method string) reshapeSpecOp {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	node, ok := doc.Paths[path][method]
	require.True(t, ok, "%s %s missing from openapi.yaml", method, path)
	var op reshapeSpecOp
	require.NoError(t, node.Decode(&op), "%s %s", method, path)
	return op
}

// TestOpenAPIReshapeOutOfScopeStatuses keeps openapi.yaml in step with what
// the AWS reshape endpoints answer when the deployment's AWS account is outside
// the session's allowed accounts (issue #628). The handler behavior itself is
// pinned by target_offerings_scope_test.go, target_offerings_zone_test.go,
// tenant_scope_sweep_test.go and the scope tests in handler_ri_exchange_test.go.
func TestOpenAPIReshapeOutOfScopeStatuses(t *testing.T) {
	offerings := loadReshapeSpecOp(t, "/api/ri-exchange/target-offerings", "get")
	for _, code := range []string{"400", "404", "422"} {
		assert.Contains(t, offerings.Responses, code, "target-offerings must document %s", code)
	}
	assert.Contains(t, offerings.Responses["404"].Description, "allowed accounts",
		"target-offerings 404 must cover an out-of-scope deployment account")
	var sourceRIRequired bool
	for _, p := range offerings.Parameters {
		if p.Name == "source_ri_id" {
			sourceRIRequired = p.Required
		}
	}
	assert.True(t, sourceRIRequired, "target-offerings must document the required source_ri_id")

	for _, path := range []string{
		"/api/ri-exchange/instances",
		"/api/ri-exchange/utilization",
		"/api/ri-exchange/reshape-recommendations",
	} {
		desc := loadReshapeSpecOp(t, path, "get").Responses["200"].Description
		assert.Contains(t, desc, "Empty when the deployment's AWS", "%s 200 must document the empty list", path)
	}

	for _, path := range []string{"/api/ri-exchange/quote", "/api/ri-exchange/execute"} {
		forbidden := loadReshapeSpecOp(t, path, "post").Responses["403"]
		assert.Empty(t, forbidden.Ref, "%s 403 must describe its causes inline", path)
		assert.Contains(t, forbidden.Description, "allowed accounts",
			"%s 403 must cover an out-of-scope deployment account", path)
	}
}
