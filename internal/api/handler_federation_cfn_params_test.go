package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

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
	res, err := federationHandler().getFederationIaC(context.Background(), federationReq(map[string]string{
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
