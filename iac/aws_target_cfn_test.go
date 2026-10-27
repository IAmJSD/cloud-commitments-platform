package iac

import (
	"fmt"
	"regexp"
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

// cfnEnv is what the evaluator below resolves references against: refs holds
// parameter values and resource attributes (keyed "Resource.Attribute", the
// way ${OIDCProvider.Arn} names them), and conds holds the template's
// evaluated Conditions for !If.
type cfnEnv struct {
	refs  map[string]string
	conds map[string]bool
}

// cfnEval evaluates the subset of CloudFormation intrinsics the trust policy
// uses (!Ref, !Select, !Split, !Sub, !If, and the !Equals and !Not its
// condition is built from) against env. Anything else is an error, so a
// rewrite into a form this cannot model fails the test instead of passing
// unevaluated.
func cfnEval(n *yaml.Node, env cfnEnv) (any, error) {
	switch n.Tag {
	case "!!str", "!!int":
		return n.Value, nil
	case "!Ref":
		v, ok := env.refs[n.Value]
		if !ok {
			return nil, fmt.Errorf("!Ref to %q, which is not a parameter value under test", n.Value)
		}
		return v, nil
	case "!Split":
		args, err := cfnArgs(n, 2, env)
		if err != nil {
			return nil, err
		}
		delim, s, ok := twoStrings(args)
		if !ok {
			return nil, fmt.Errorf("!Split arguments are not strings: %v", args)
		}
		return strings.Split(s, delim), nil
	case "!Select":
		args, err := cfnArgs(n, 2, env)
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
		return cfnSub(n, env)
	case "!If":
		return cfnIf(n, env)
	case "!Equals":
		args, err := cfnArgs(n, 2, env)
		if err != nil {
			return nil, err
		}
		a, b, ok := twoStrings(args)
		if !ok {
			return nil, fmt.Errorf("!Equals arguments are not strings: %v", args)
		}
		return a == b, nil
	case "!Not":
		args, err := cfnArgs(n, 1, env)
		if err != nil {
			return nil, err
		}
		b, ok := args[0].(bool)
		if !ok {
			return nil, fmt.Errorf("!Not argument is not a condition: %v", args[0])
		}
		return !b, nil
	}
	return nil, fmt.Errorf("unsupported node tag %q (value %q)", n.Tag, n.Value)
}

// cfnArgs evaluates the elements of a sequence-form intrinsic.
func cfnArgs(n *yaml.Node, want int, env cfnEnv) ([]any, error) {
	if n.Kind != yaml.SequenceNode || len(n.Content) != want {
		return nil, fmt.Errorf("%s wants a %d-element list", n.Tag, want)
	}
	out := make([]any, 0, want)
	for _, c := range n.Content {
		v, err := cfnEval(c, env)
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

// cfnIf evaluates only the branch its named condition selects, as
// CloudFormation does.
func cfnIf(n *yaml.Node, env cfnEnv) (any, error) {
	if n.Kind != yaml.SequenceNode || len(n.Content) != 3 || n.Content[0].Tag != "!!str" {
		return nil, fmt.Errorf("!If wants [condition name, value if true, value if false]")
	}
	cond, ok := env.conds[n.Content[0].Value]
	if !ok {
		return nil, fmt.Errorf("!If names condition %q, which the template does not define", n.Content[0].Value)
	}
	if cond {
		return cfnEval(n.Content[1], env)
	}
	return cfnEval(n.Content[2], env)
}

// cfnConditions evaluates the template's Conditions section for refs.
func cfnConditions(t *testing.T, root *yaml.Node, refs map[string]string) map[string]bool {
	t.Helper()
	section := mapPath(t, root, "Conditions")
	conds := map[string]bool{}
	for i := 0; i+1 < len(section.Content); i += 2 {
		v, err := cfnEval(section.Content[i+1], cfnEnv{refs: refs})
		require.NoError(t, err, "condition %s", section.Content[i].Value)
		b, ok := v.(bool)
		require.True(t, ok, "condition %s does not evaluate to a boolean", section.Content[i].Value)
		conds[section.Content[i].Value] = b
	}
	return conds
}

var subVarRe = regexp.MustCompile(`\$\{([^}!]+)\}`)

// cfnSub renders both the string and the [string, {vars}] forms of !Sub. Like
// CloudFormation it substitutes in a single pass: text a variable expands to
// is not scanned for further ${...} references.
func cfnSub(n *yaml.Node, env cfnEnv) (any, error) {
	format, vars := n, map[string]string{}
	if n.Kind == yaml.SequenceNode {
		if len(n.Content) != 2 || n.Content[1].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("!Sub list form wants [string, mapping]")
		}
		format = n.Content[0]
		m := n.Content[1]
		for i := 0; i+1 < len(m.Content); i += 2 {
			v, err := cfnEval(m.Content[i+1], env)
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
		if v, ok := env.refs[name]; ok {
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

// TestCFNEval_Positive is the control for the evaluator above: it must
// resolve the derivation and the condition the template uses, and reject an
// unknown parameter or condition, so the trust-policy tests cannot pass by
// evaluating nothing.
func TestCFNEval_Positive(t *testing.T) {
	env := cfnEnv{refs: map[string]string{"OIDCIssuerURL": "https://accounts.google.com"}}
	eval := func(src string, env cfnEnv) (any, error) {
		var n yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte(src), &n))
		return cfnEval(n.Content[0], env)
	}

	got, err := eval(`!Sub ["${Host}:aud", {Host: !Select [1, !Split ["https://", !Ref OIDCIssuerURL]]}]`, env)
	require.NoError(t, err)
	require.Equal(t, "accounts.google.com:aud", got)

	_, err = eval(`!Sub "${OIDCIssuerHost}:aud"`, env)
	require.Error(t, err)

	for aud, want := range map[string]bool{"": false, "api://x": true} {
		got, err = eval(`!Not [!Equals [!Ref OIDCAudience, ""]]`, cfnEnv{refs: map[string]string{"OIDCAudience": aud}})
		require.NoError(t, err)
		require.Equal(t, want, got, "audience %q", aud)
	}

	ifSrc := `!If [HasAudience, !Ref OIDCAudience, sts.amazonaws.com]`
	refs := map[string]string{"OIDCAudience": "api://x"}
	got, err = eval(ifSrc, cfnEnv{refs: refs, conds: map[string]bool{"HasAudience": true}})
	require.NoError(t, err)
	require.Equal(t, "api://x", got)
	got, err = eval(ifSrc, cfnEnv{refs: refs, conds: map[string]bool{"HasAudience": false}})
	require.NoError(t, err)
	require.Equal(t, "sts.amazonaws.com", got)
	_, err = eval(ifSrc, cfnEnv{refs: refs})
	require.Error(t, err, "an !If on an undefined condition must not evaluate")

	// Single-pass substitution: a value containing ${...} is not re-expanded.
	got, err = eval(`!Sub ["${A}", {A: "${B}"}]`, cfnEnv{refs: map[string]string{"B": "expanded"}})
	require.NoError(t, err)
	require.Equal(t, "${B}", got)
}
