package api

import (
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// riExchangeSpecSchema is the slice of an inline openapi.yaml schema these
// checks read.
type riExchangeSpecSchema struct {
	Type       string   `yaml:"type"`
	Required   []string `yaml:"required"`
	Properties map[string]struct {
		Type        string `yaml:"type"`
		Description string `yaml:"description"`
	} `yaml:"properties"`
}

type riExchangeSpecMedia struct {
	Content map[string]struct {
		Schema riExchangeSpecSchema `yaml:"schema"`
	} `yaml:"content"`
}

type riExchangeSpecOp struct {
	RequestBody riExchangeSpecMedia            `yaml:"requestBody"`
	Responses   map[string]riExchangeSpecMedia `yaml:"responses"`
}

func loadRIExchangeSpecOp(t *testing.T, path string) riExchangeSpecOp {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(openapiSpec, &spec))
	node, ok := spec.Paths[path]["post"]
	require.True(t, ok, "post %s missing from openapi.yaml", path)
	var op riExchangeSpecOp
	require.NoError(t, node.Decode(&op), "post %s", path)
	return op
}

// jsonKeys returns the top-level keys v marshals to, so the spec checks
// follow the Go json tags rather than a hand-copied field name.
func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &m))
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// TestOpenAPIRIExchangeRegionRequired keeps openapi.yaml in step with the
// handler on the exchange region (issue #363): execute refuses a request
// without a region (validateExecuteExchangeBody), and the quote response
// always carries the region the quote was priced in, which execute must send
// back.
func TestOpenAPIRIExchangeRegionRequired(t *testing.T) {
	// The handler side of the contract: a body that is otherwise valid but
	// names no region is a 400.
	err := validateExecuteExchangeBody(ExchangeExecuteRequestBody{
		RIIDs:            []string{"ri-1"},
		Targets:          []ExchangeTargetBody{{OfferingID: "4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91", Count: 1}},
		MaxPaymentDueUSD: "100.00",
	})
	var ce *clientError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.message, "region is required")

	execKeys := jsonKeys(t, ExchangeExecuteRequestBody{Region: "us-west-2"})
	require.True(t, execKeys["region"], "execute body's region json key changed; update this test and openapi.yaml")

	execSchema, ok := loadRIExchangeSpecOp(t, "/api/ri-exchange/execute").RequestBody.Content["application/json"]
	require.True(t, ok, "execute must document an application/json request body")
	assert.Contains(t, execSchema.Schema.Required, "region", "execute must list region as required")
	require.Contains(t, execSchema.Schema.Properties, "region", "execute must document the region property")
	assert.Equal(t, "string", execSchema.Schema.Properties["region"].Type)
	assert.NotEmpty(t, execSchema.Schema.Properties["region"].Description)

	quoteKeys := jsonKeys(t, ExchangeQuoteResponse{
		ExchangeQuoteSummary: &exchange.ExchangeQuoteSummary{},
		Region:               "us-west-2",
	})
	require.True(t, quoteKeys["Region"], "quote response's Region json key changed; update this test and openapi.yaml")

	quoteSchema, ok := loadRIExchangeSpecOp(t, "/api/ri-exchange/quote").Responses["200"].Content["application/json"]
	require.True(t, ok, "quote must document an application/json 200 response")
	assert.Equal(t, "object", quoteSchema.Schema.Type)
	assert.Contains(t, quoteSchema.Schema.Required, "Region", "quote response must list Region as always present")
	require.Contains(t, quoteSchema.Schema.Properties, "Region", "quote response must document Region")
	assert.Equal(t, "string", quoteSchema.Schema.Properties["Region"].Type)
	assert.Contains(t, quoteSchema.Schema.Properties["Region"].Description, "execute",
		"quote Region must tell callers to send it back on execute")
}
