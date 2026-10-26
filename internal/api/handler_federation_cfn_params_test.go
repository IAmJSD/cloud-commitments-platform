package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// cfnOverrideRe matches one "Key=value" argument of the deploy script's
// --parameter-overrides list.
var cfnOverrideRe = regexp.MustCompile(`(?m)^\s+"([A-Za-z][A-Za-z0-9]*)=`)

// cfnBundleFiles returns the template, the parameter keys from the params
// JSON, and the parameter keys the deploy script overrides, from a format=cfn
// download.
func cfnBundleFiles(t *testing.T, source string) (template []byte, paramsKeys, scriptKeys []string) {
	t.Helper()
	res, err := federationHandler(t).getFederationIaC(context.Background(), federationReq(map[string]string{
		"target": "aws", "source": source, "format": "cfn",
	}))
	require.NoError(t, err)
	raw, err := base64.StdEncoding.DecodeString(res.Content)
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		switch {
		case f.Name == "cloudformation/template.yaml":
			template = body
		case strings.HasSuffix(f.Name, "-cf-params.json"):
			var params []map[string]string
			require.NoError(t, json.Unmarshal(body, &params))
			for _, p := range params {
				paramsKeys = append(paramsKeys, p["ParameterKey"])
			}
		case f.Name == "cloudformation/deploy-cfn.sh":
			for _, m := range cfnOverrideRe.FindAllStringSubmatch(string(body), -1) {
				scriptKeys = append(scriptKeys, m[1])
			}
		}
	}
	require.NotEmpty(t, template, "bundle has no template")
	require.NotEmpty(t, paramsKeys, "bundle has no params JSON entries")
	require.NotEmpty(t, scriptKeys, "deploy script has no --parameter-overrides entries")
	return template, paramsKeys, scriptKeys
}

// cfnTemplateParameters returns the template's declared parameters, mapped to
// whether each one has a Default.
func cfnTemplateParameters(t *testing.T, template []byte) map[string]bool {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal(template, &doc))
	root := doc.Content[0]
	var params *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "Parameters" {
			params = root.Content[i+1]
		}
	}
	require.NotNil(t, params, "template has no Parameters section")
	out := map[string]bool{}
	for i := 0; i+1 < len(params.Content); i += 2 {
		hasDefault := false
		body := params.Content[i+1]
		for j := 0; j+1 < len(body.Content); j += 2 {
			if body.Content[j].Value == "Default" {
				hasDefault = true
			}
		}
		out[params.Content[i].Value] = hasDefault
	}
	return out
}

// TestCFNBundle_OverridesMatchTemplateParameters guards the contract between
// the CloudFormation template a bundle ships and the two ways the bundle
// passes it parameters. CloudFormation rejects a parameter the template does
// not declare, so a key left behind after a parameter is removed (as
// OIDCIssuerHost was, #324) breaks every deploy, and a missing required one
// fails it too.
func TestCFNBundle_OverridesMatchTemplateParameters(t *testing.T) {
	// Both non-AWS sources ship the aws-target WIF template.
	for _, source := range []string{"gcp", "azure"} {
		t.Run(source, func(t *testing.T) {
			template, paramsKeys, scriptKeys := cfnBundleFiles(t, source)
			declared := cfnTemplateParameters(t, template)
			for name, keys := range map[string][]string{"params JSON": paramsKeys, "deploy script": scriptKeys} {
				passed := map[string]bool{}
				for _, k := range keys {
					require.Contains(t, declared, k, "%s passes %s, which the template does not declare", name, k)
					passed[k] = true
				}
				var missing []string
				for p, hasDefault := range declared {
					if !hasDefault && !passed[p] {
						missing = append(missing, p)
					}
				}
				sort.Strings(missing)
				require.Empty(t, missing, "%s omits required template parameters", name)
			}
			require.NotContains(t, declared, "OIDCIssuerHost")
			require.Contains(t, paramsKeys, "OIDCIssuerURL")
			require.Contains(t, scriptKeys, "OIDCIssuerURL")
		})
	}
}

// TestCFNBundle_TemplatePassesCFNLint covers #1489's acceptance check on the
// bundle a customer actually downloads, not only the in-repo evaluator in
// iac/aws_target_cfn_trust_policy_test.go: the aws-target template once
// rendered fine there yet could not be loaded by CloudFormation at all,
// because its trust policy used intrinsic functions as map keys.
//
// The byte comparison always runs. It is what lets the cfn-lint pre-commit
// hook, which lints the committed file, vouch for every downloaded bundle. The
// cfn-lint run itself needs cfn-lint on PATH (pip install cfn-lint) and skips
// without it, the same way the Terraform bundle test skips without terraform.
// The aws-target template must lint with no findings at all, warnings
// included.
func TestCFNBundle_TemplatePassesCFNLint(t *testing.T) {
	committed, err := os.ReadFile(awsTargetCFNTemplate)
	require.NoError(t, err)
	for _, source := range []string{"gcp", "azure"} {
		t.Run(source, func(t *testing.T) {
			template, _, _ := cfnBundleFiles(t, source)
			require.Equal(t, string(committed), string(template),
				"the bundle must ship the committed aws-target template byte for byte, so linting that file covers the download")

			cfnLint, err := exec.LookPath("cfn-lint")
			if err != nil {
				t.Skip("cfn-lint not on PATH")
			}
			path := filepath.Join(t.TempDir(), "template.yaml")
			require.NoError(t, os.WriteFile(path, template, 0o600))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := exec.CommandContext(ctx, cfnLint, path).CombinedOutput()
			require.NoError(t, err, "cfn-lint rejects the template in the %s-source AWS-target bundle:\n%s", source, out)
		})
	}
}
