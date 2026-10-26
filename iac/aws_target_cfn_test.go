package iac

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Regression tests for #324 (audit finding A13b-014). The aws-target
// CloudFormation template used to take the trust policy's condition-key host
// as a second free-text parameter, OIDCIssuerHost, that the operator had to
// keep equal to OIDCIssuerURL minus https:// by hand. A mismatch produced a
// stack that deployed cleanly and then denied every AssumeRoleWithWebIdentity.
// The host is now derived from OIDCIssuerURL, the way the Terraform sibling's
// local.oidc_condition_host derives it.

const awsTargetCFNTemplate = "federation/aws-target/cloudformation/template.yaml"

// mapValue returns the value node for key in a YAML mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Kind == yaml.ScalarNode && n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mapPath follows keys from n through nested mappings, failing the test when
// any step is missing.
func mapPath(t *testing.T, n *yaml.Node, keys ...string) *yaml.Node {
	t.Helper()
	for _, k := range keys {
		next := mapValue(n, k)
		require.NotNil(t, next, "template has no %q under the path %v", k, keys)
		n = next
	}
	return n
}

func loadAWSTargetCFN(t *testing.T) *yaml.Node {
	t.Helper()
	raw, err := Modules.ReadFile(awsTargetCFNTemplate)
	require.NoError(t, err)
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.Len(t, doc.Content, 1)
	return doc.Content[0]
}

// cfnEval evaluates the subset of CloudFormation intrinsics the trust
// policy's condition keys use (!Ref, !Select, !Split, !Sub) against the given
// parameter values. Anything else is an error, so a rewrite into a form this
// cannot model fails the test instead of passing unevaluated.
func cfnEval(n *yaml.Node, params map[string]string) (any, error) {
	switch n.Tag {
	case "!!str", "!!int":
		return n.Value, nil
	case "!Ref":
		v, ok := params[n.Value]
		if !ok {
			return nil, fmt.Errorf("!Ref to %q, which is not a parameter value under test", n.Value)
		}
		return v, nil
	case "!Split":
		args, err := cfnArgs(n, 2, params)
		if err != nil {
			return nil, err
		}
		delim, s, ok := twoStrings(args)
		if !ok {
			return nil, fmt.Errorf("!Split arguments are not strings: %v", args)
		}
		return strings.Split(s, delim), nil
	case "!Select":
		args, err := cfnArgs(n, 2, params)
		if err != nil {
			return nil, err
		}
		idx, err := strconv.Atoi(fmt.Sprint(args[0]))
		list, ok := args[1].([]string)
		if err != nil || !ok || idx < 0 || idx >= len(list) {
			return nil, fmt.Errorf("!Select %v out of range or not a list", args)
		}
		return list[idx], nil
	case "!Sub":
		return cfnSub(n, params)
	}
	return nil, fmt.Errorf("unsupported node tag %q (value %q)", n.Tag, n.Value)
}

// cfnArgs evaluates the elements of a sequence-form intrinsic.
func cfnArgs(n *yaml.Node, want int, params map[string]string) ([]any, error) {
	if n.Kind != yaml.SequenceNode || len(n.Content) != want {
		return nil, fmt.Errorf("%s wants a %d-element list", n.Tag, want)
	}
	out := make([]any, 0, want)
	for _, c := range n.Content {
		v, err := cfnEval(c, params)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func twoStrings(args []any) (string, string, bool) {
	a, ok1 := args[0].(string)
	b, ok2 := args[1].(string)
	return a, b, ok1 && ok2
}

var subVarRe = regexp.MustCompile(`\$\{([^}!]+)\}`)

// cfnSub renders both the string and the [string, {vars}] forms of !Sub.
func cfnSub(n *yaml.Node, params map[string]string) (any, error) {
	format, vars := n, map[string]string{}
	if n.Kind == yaml.SequenceNode {
		if len(n.Content) != 2 || n.Content[1].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("!Sub list form wants [string, mapping]")
		}
		format = n.Content[0]
		m := n.Content[1]
		for i := 0; i+1 < len(m.Content); i += 2 {
			v, err := cfnEval(m.Content[i+1], params)
			if err != nil {
				return nil, err
			}
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("!Sub variable %q is not a string", m.Content[i].Value)
			}
			vars[m.Content[i].Value] = s
		}
	}
	var missing error
	out := subVarRe.ReplaceAllStringFunc(format.Value, func(ref string) string {
		name := subVarRe.FindStringSubmatch(ref)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		if v, ok := params[name]; ok {
			return v
		}
		missing = fmt.Errorf("!Sub references %q, which is neither a variable nor a parameter value under test", name)
		return ref
	})
	return out, missing
}

// terraformConditionHost mirrors local.oidc_condition_host in
// federation/aws-target/terraform/main.tf.
func terraformConditionHost(url string) string {
	return strings.TrimSuffix(strings.TrimPrefix(url, "https://"), "/")
}

func TestAWSTargetCFN_HasNoIssuerHostParameter(t *testing.T) {
	root := loadAWSTargetCFN(t)
	params := mapPath(t, root, "Parameters")
	require.Nil(t, mapValue(params, "OIDCIssuerHost"),
		"OIDCIssuerHost must not be a parameter: the condition-key host is derived from OIDCIssuerURL (#324)")
	raw, err := Modules.ReadFile(awsTargetCFNTemplate)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "${OIDCIssuerHost}", "nothing may still reference the removed parameter")
}

// TestAWSTargetCFN_ConditionKeysDeriveFromIssuerURL evaluates every key of the
// trust policy's StringEquals block for real issuer URLs and checks it is the
// host Terraform derives, followed by :aud or :sub.
func TestAWSTargetCFN_ConditionKeysDeriveFromIssuerURL(t *testing.T) {
	root := loadAWSTargetCFN(t)
	urlPattern := regexp.MustCompile(mapPath(t, root, "Parameters", "OIDCIssuerURL", "AllowedPattern").Value)
	statements := mapPath(t, root, "Resources", "CUDlyRole", "Properties", "AssumeRolePolicyDocument", "Statement")
	require.Equal(t, yaml.SequenceNode, statements.Kind)
	require.Len(t, statements.Content, 1, "the trust policy has exactly one statement")
	stringEquals := mapPath(t, statements.Content[0], "Condition", "StringEquals")

	for _, url := range []string{
		"https://accounts.google.com",
		"https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0",
	} {
		t.Run(url, func(t *testing.T) {
			require.True(t, urlPattern.MatchString(url), "fixture must satisfy OIDCIssuerURL's AllowedPattern")
			host := terraformConditionHost(url)
			var keys []string
			for i := 0; i+1 < len(stringEquals.Content); i += 2 {
				v, err := cfnEval(stringEquals.Content[i], map[string]string{"OIDCIssuerURL": url})
				require.NoError(t, err, "condition key %d must be computed from OIDCIssuerURL alone", i/2)
				keys = append(keys, v.(string))
			}
			sort.Strings(keys)
			require.Equal(t, []string{host + ":aud", host + ":sub"}, keys)
		})
	}
}

// TestCFNEval_Positive is the control for the evaluator above: it must
// resolve the derivation the template uses, and reject an unknown parameter,
// so the condition-key test cannot pass by evaluating nothing.
func TestCFNEval_Positive(t *testing.T) {
	var n yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`!Sub ["${Host}:aud", {Host: !Select [1, !Split ["https://", !Ref OIDCIssuerURL]]}]`), &n))
	got, err := cfnEval(n.Content[0], map[string]string{"OIDCIssuerURL": "https://accounts.google.com"})
	require.NoError(t, err)
	require.Equal(t, "accounts.google.com:aud", got)

	require.NoError(t, yaml.Unmarshal([]byte(`!Sub "${OIDCIssuerHost}:aud"`), &n))
	_, err = cfnEval(n.Content[0], map[string]string{"OIDCIssuerURL": "https://accounts.google.com"})
	require.Error(t, err)
}
