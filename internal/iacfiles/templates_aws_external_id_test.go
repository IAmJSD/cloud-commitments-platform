package iacfiles

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// minExternalIDLen is the shortest external ID the CloudFormation templates
// may accept: a 16-byte hex string (`openssl rand -hex 16`). UUIDs are 36.
const minExternalIDLen = 32

type cfnStringParameter struct {
	Type           string `yaml:"Type"`
	MinLength      int    `yaml:"MinLength"`
	AllowedPattern string `yaml:"AllowedPattern"`
}

// cfnParameter decodes one entry of a CloudFormation template's Parameters
// block. It walks yaml.Node so intrinsic-function tags (!Ref, !Sub) elsewhere
// in the template do not get in the way.
func cfnParameter(t *testing.T, rel, name string) cfnStringParameter {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", rel))
	require.NoError(t, err, rel)
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal(raw, &root), rel)
	require.NotEmpty(t, root.Content, rel)
	params := mappingValue(root.Content[0], "Parameters")
	require.NotNil(t, params, "%s has no Parameters block", rel)
	node := mappingValue(params, name)
	require.NotNil(t, node, "%s has no %s parameter", rel, name)
	var p cfnStringParameter
	require.NoError(t, node.Decode(&p), rel)
	return p
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// cfnAccepts reports whether CloudFormation would accept value for p, applying
// the MinLength and AllowedPattern checks CloudFormation applies to a String
// parameter. The pattern is compiled as written, so a template must anchor it
// itself rather than rely on CloudFormation matching the whole value.
func cfnAccepts(t *testing.T, p cfnStringParameter, value string) bool {
	t.Helper()
	if len(value) < p.MinLength {
		return false
	}
	if p.AllowedPattern == "" {
		return true
	}
	re, err := regexp.Compile(p.AllowedPattern)
	require.NoError(t, err, "AllowedPattern must compile: %q", p.AllowedPattern)
	return re.MatchString(value)
}

// TestCFNExternalIDParameterRequiresStrongValue pins #290: both CloudFormation
// cross-account templates must refuse a short or guessable sts:ExternalId
// (the Terraform module for the same trust generates a random_uuid), while
// still accepting every value CUDly's own onboarding paths generate.
func TestCFNExternalIDParameterRequiresStrongValue(t *testing.T) {
	templates := map[string]string{
		"iac/federation/aws-cross-account/cloudformation/template.yaml": "ExternalID",
		"cloudformation/stacks/CUDly-CrossAccount/template.yaml":        "ExternalId",
	}

	generated := map[string]string{
		// handler_federation.go buildCFParamsJSON and Terraform random_uuid.
		"google/uuid lowercase UUID": uuid.New().String(),
		// deploy-cfn.sh falls back to uuidgen, which prints uppercase on macOS.
		"uuidgen uppercase UUID": strings.ToUpper(uuid.New().String()),
		// The CUDly-CrossAccount description says `openssl rand -hex 16`.
		"openssl rand -hex 16": strings.ReplaceAll(uuid.New().String(), "-", ""),
	}

	weak := map[string]string{
		"issue example password":        "password",
		"issue example cudly123":        "cudly123",
		"12-digit account number":       "123456789012",
		"32 chars, not hex":             "passwordpasswordpasswordpassword",
		"31 hex chars":                  strings.Repeat("a", 31),
		"33 hex chars":                  strings.Repeat("a", 33),
		"UUID with a trailing newline":  uuid.New().String() + "\n",
		"UUID with a prefix":            "x" + uuid.New().String(),
		"UUID shape with non-hex digit": "g" + uuid.New().String()[1:],
		"empty":                         "",
	}

	for rel, name := range templates {
		t.Run(rel, func(t *testing.T) {
			p := cfnParameter(t, rel, name)
			assert.Equal(t, "String", p.Type)
			assert.GreaterOrEqual(t, p.MinLength, minExternalIDLen,
				"%s.MinLength must be at least %d", name, minExternalIDLen)
			require.NotEmpty(t, p.AllowedPattern, "%s must declare an AllowedPattern", name)

			for label, v := range generated {
				assert.True(t, cfnAccepts(t, p, v), "%s must accept %s (%q)", name, label, v)
			}
			for label, v := range weak {
				assert.False(t, cfnAccepts(t, p, v), "%s must reject %s (%q)", name, label, v)
			}
		})
	}
}
