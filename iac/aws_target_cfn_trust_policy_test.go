package iac

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Regression tests for #1489. The aws-target template built its trust policy
// as a YAML mapping whose two StringEquals keys were !Sub expressions (the
// host is derived from OIDCIssuerURL, #324). CloudFormation does not accept an
// intrinsic function as a map key, so the template could not be loaded at all
// (cfn-lint E0000 "Unhashable type") and deploy-cfn.sh created no stack. The
// document is now a single !Sub JSON string. Fn::Sub inserts raw text, so
// these tests also prove that no value the parameters' AllowedPatterns admit
// can escape the JSON string it lands in and change the policy.

// trustPolicyProviderARN stands in for ${OIDCProvider.Arn}. It is not a real
// ARN; the tests only need it to come back out of the rendered JSON unchanged.
const trustPolicyProviderARN = "OIDC-PROVIDER-ARN-PLACEHOLDER"

// trustPolicyDefaults are valid values for every parameter the trust policy
// reads, so each test can vary one at a time.
func trustPolicyDefaults() map[string]string {
	return map[string]string{
		"OIDCIssuerURL":    "https://accounts.google.com",
		"OIDCAudience":     "api://11111111-1111-1111-1111-111111111111",
		"OIDCSubjectClaim": "123456789012345678901",
		"OIDCThumbprint":   "",
		"RoleName":         "CUDly-WIF",
	}
}

// renderTrustPolicy evaluates CUDlyRole's AssumeRolePolicyDocument for params
// and returns the JSON text CloudFormation would hand to IAM.
func renderTrustPolicy(t *testing.T, root *yaml.Node, params map[string]string) (string, error) {
	t.Helper()
	doc := mapPath(t, root, "Resources", "CUDlyRole", "Properties", "AssumeRolePolicyDocument")
	require.Equal(t, "!Sub", doc.Tag, "the trust policy must be a single !Sub JSON string (#1489)")
	refs := map[string]string{"OIDCProvider.Arn": trustPolicyProviderARN}
	for k, v := range params {
		refs[k] = v
	}
	v, err := cfnEval(doc, cfnEnv{refs: refs, conds: cfnConditions(t, root, refs)})
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	require.True(t, ok, "the trust policy must render to a string")
	return s, nil
}

// wantTrustPolicy is the exact policy the template must produce: one Allow
// statement for the OIDC provider, sts:AssumeRoleWithWebIdentity only, and a
// StringEquals block holding exactly the :aud and :sub keys. Comparing the
// whole document means an added statement, operator, key or principal fails
// the test, not only a changed value.
func wantTrustPolicy(host, aud, sub string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]any{"Federated": trustPolicyProviderARN},
			"Action":    "sts:AssumeRoleWithWebIdentity",
			"Condition": map[string]any{"StringEquals": map[string]any{
				host + ":aud": aud,
				host + ":sub": sub,
			}},
		}},
	}
}

// decodeStrictJSON decodes a single JSON document and, unlike json.Unmarshal,
// rejects duplicate object keys and trailing data. A duplicate key is how an
// injected value would override a condition (IAM's handling of duplicates is
// not something to rely on), so it must not collapse silently.
func decodeStrictJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	v, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	switch extra, err := dec.Token(); {
	case errors.Is(err, io.EOF):
		return v, nil
	case err != nil:
		return nil, fmt.Errorf("after the JSON document: %w", err)
	default:
		return nil, fmt.Errorf("data after the JSON document: %v", extra)
	}
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		obj := map[string]any{}
		for dec.More() {
			kt, kerr := dec.Token()
			if kerr != nil {
				return nil, kerr
			}
			k := kt.(string)
			if _, dup := obj[k]; dup {
				return nil, fmt.Errorf("duplicate key %q", k)
			}
			if obj[k], err = decodeJSONValue(dec); err != nil {
				return nil, err
			}
		}
		_, err = dec.Token()
		return obj, err
	case json.Delim('['):
		arr := []any{}
		for dec.More() {
			v, verr := decodeJSONValue(dec)
			if verr != nil {
				return nil, verr
			}
			arr = append(arr, v)
		}
		_, err = dec.Token()
		return arr, err
	}
	return tok, nil
}

// TestDecodeStrictJSON_Control proves the decoder the trust-policy tests rely
// on rejects what json.Unmarshal would let through silently.
func TestDecodeStrictJSON_Control(t *testing.T) {
	got, err := decodeStrictJSON(`{"a": ["b", {"c": "d"}]}`)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"a": []any{"b", map[string]any{"c": "d"}}}, got)
	for _, bad := range []string{
		`{"a": "b", "a": "c"}`,
		`{"a": {"b": "c", "b": "d"}}`,
		`{"a": "b"} {"a": "c"}`,
		`{"a": "b"}]`,
		"{\"a\": \"b\x01\"}",
	} {
		_, err := decodeStrictJSON(bad)
		require.Error(t, err, "%q", bad)
	}
}

// paramAdmits reports whether CloudFormation would accept value for the named
// parameter: its AllowedPattern as a whole-string match (Java's
// Matcher.matches(), which is what CloudFormation applies) and its MinLength,
// where the parameter declares them.
func paramAdmits(t *testing.T, root *yaml.Node, name, value string) bool {
	t.Helper()
	param := mapPath(t, root, "Parameters", name)
	if minLen := mapValue(param, "MinLength"); minLen != nil {
		n, err := strconv.Atoi(minLen.Value)
		require.NoError(t, err)
		if len(value) < n {
			return false
		}
	}
	allowed := mapValue(param, "AllowedPattern")
	if allowed == nil {
		return true
	}
	return regexp.MustCompile(`^(?:` + allowed.Value + `)$`).MatchString(value)
}

// TestAWSTargetCFN_MappingKeysArePlainStrings is the load-failure regression
// itself: CloudFormation rejects the whole template when any mapping key is an
// intrinsic function or other non-string node, as the trust policy's condition
// keys were. It walks every mapping in the template.
func TestAWSTargetCFN_MappingKeysArePlainStrings(t *testing.T) {
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				require.True(t, k.Kind == yaml.ScalarNode && k.Tag == "!!str",
					"mapping key under %s at line %d is a %s node tagged %q; CloudFormation cannot load a template whose map keys are not plain strings (#1489)",
					path, k.Line, kindName(k.Kind), k.Tag)
				walk(n.Content[i+1], path+"/"+k.Value)
			}
			return
		}
		for _, c := range n.Content {
			walk(c, path)
		}
	}
	walk(loadAWSTargetCFN(t), "")
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	}
	return fmt.Sprintf("kind-%d", k)
}

// TestAWSTargetCFN_ConditionKeysDeriveFromIssuerURL renders the trust policy
// for real issuer URLs, with and without an audience, and checks it parses to
// exactly the intended policy: condition keys built from the host Terraform
// derives, the audience falling back to sts.amazonaws.com when empty, and the
// subject pinned.
func TestAWSTargetCFN_ConditionKeysDeriveFromIssuerURL(t *testing.T) {
	root := loadAWSTargetCFN(t)
	for _, url := range []string{
		"https://accounts.google.com",
		"https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0",
	} {
		for _, aud := range []string{"", "api://11111111-1111-1111-1111-111111111111"} {
			t.Run(url+"/aud="+aud, func(t *testing.T) {
				params := trustPolicyDefaults()
				params["OIDCIssuerURL"], params["OIDCAudience"] = url, aud
				for name, v := range params {
					require.True(t, paramAdmits(t, root, name, v), "fixture %s=%q must satisfy the parameter's constraints", name, v)
				}
				rendered, err := renderTrustPolicy(t, root, params)
				require.NoError(t, err)
				got, err := decodeStrictJSON(rendered)
				require.NoError(t, err, "rendered trust policy is not valid JSON:\n%s", rendered)

				wantAud := aud
				if wantAud == "" {
					wantAud = "sts.amazonaws.com"
				}
				require.Equal(t, wantTrustPolicy(terraformConditionHost(url), wantAud, params["OIDCSubjectClaim"]), got)
			})
		}
	}
}

// probeValues returns, for each parameter the trust policy reads, values that
// carry every ASCII character and a few non-ASCII ones at each place the
// parameter's pattern treats differently (the URL's host and path segments).
func probeValues() map[string][]string {
	runes := make([]rune, 0, 132)
	for r := rune(0); r < 0x80; r++ {
		runes = append(runes, r)
	}
	runes = append(runes, ' ', 'é', ' ', ' ')
	out := map[string][]string{}
	for _, r := range runes {
		c := string(r)
		out["OIDCIssuerURL"] = append(out["OIDCIssuerURL"],
			"https://acc"+c+"ounts.google.com", "https://login.microsoftonline.com/te"+c+"nant/v2.0")
		out["OIDCAudience"] = append(out["OIDCAudience"], "api://a"+c+"b")
		out["OIDCSubjectClaim"] = append(out["OIDCSubjectClaim"], "12"+c+"34")
	}
	return out
}

// TestAWSTargetCFN_TrustPolicyValuesCannotEscapeJSON is the injection
// property: for every probe value the parameter's constraints admit, the
// rendered document must still decode to exactly the intended policy with that
// value in its place. A '"' or '\' or control character that got through would
// make the JSON invalid, re-escape the value, or add a key, and fail here.
func TestAWSTargetCFN_TrustPolicyValuesCannotEscapeJSON(t *testing.T) {
	root := loadAWSTargetCFN(t)
	for name, values := range probeValues() {
		t.Run(name, func(t *testing.T) {
			admitted := 0
			for _, v := range values {
				if !paramAdmits(t, root, name, v) {
					continue
				}
				admitted++
				params := trustPolicyDefaults()
				params[name] = v
				rendered, err := renderTrustPolicy(t, root, params)
				require.NoError(t, err)
				got, err := decodeStrictJSON(rendered)
				require.NoError(t, err, "%s=%q is admitted but breaks the trust policy JSON:\n%s", name, v, rendered)
				want := wantTrustPolicy(terraformConditionHost(params["OIDCIssuerURL"]), params["OIDCAudience"], params["OIDCSubjectClaim"])
				require.Equal(t, want, got, "%s=%q is admitted but changes the trust policy", name, v)
			}
			require.Positive(t, admitted, "the probe must exercise admitted values, not only rejected ones")
		})
	}
}

// TestAWSTargetCFN_RejectsJSONBreakout pins the characters that would let a
// value leave its JSON string, and whole breakout payloads, as rejected at
// submission time for every parameter the trust policy reads.
func TestAWSTargetCFN_RejectsJSONBreakout(t *testing.T) {
	root := loadAWSTargetCFN(t)
	cases := map[string][]string{
		"OIDCIssuerURL": {
			`https://accounts.google.com"`,
			`https://accounts.google.com/a\b`,
			"https://accounts.google.com/a\nb",
			`https://x:aud":"y","x:sub":"z"},"StringLike":{"accounts.google.com`,
		},
		"OIDCAudience": {
			`api://x"`,
			`api://x"`,
			"api://x\tb",
			`x","accounts.google.com:sub":"other`,
		},
		"OIDCSubjectClaim": {
			`123"`,
			`123"`,
			"123\x00",
			`1"}},{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::other:root"},"Action":"sts:AssumeRole`,
		},
	}
	for name, values := range cases {
		for _, v := range values {
			require.False(t, paramAdmits(t, root, name, v), "%s must reject %q", name, v)
		}
	}
}
