package api

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// federationBundleCombos lists every target/source pair that renders a
// distinct Terraform bundle, with the CUDLY_SOURCE_CLOUD each needs to pass
// validateFederationTargetSource.
var federationBundleCombos = []struct {
	target, source, sourceCloud, module string
}{
	{"aws", "aws", "aws", "aws-cross-account"},
	{"aws", "gcp", "gcp", "aws-target"},
	{"azure", "gcp", "gcp", "azure-target"},
	{"gcp", "gcp", "gcp", "gcp-sa-impersonation"},
	{"gcp", "aws", "aws", "gcp-target"},
}

// renderBundleFiles renders a bundle through the real API path and returns
// its files.
func renderBundleFiles(t *testing.T, target, source, sourceCloud string) map[string][]byte {
	t.Helper()
	t.Setenv("CUDLY_SOURCE_CLOUD", sourceCloud)
	h := federationHandler()
	res, err := h.getFederationIaC(context.Background(), federationReq(map[string]string{
		"target": target, "source": source, "format": "bundle",
	}))
	require.NoError(t, err)
	return unzipResponse(t, res)
}

// versionBounds is a constraint reduced to its lower and upper bound, each a
// normalized version with its operator ("" when unbounded).
type versionBounds struct {
	Floor   string
	Ceiling string
}

func (b versionBounds) String() string {
	return fmt.Sprintf("floor=%q ceiling=%q", b.Floor, b.Ceiling)
}

// normalizeVersion pads a numeric version to three segments, so "5" and
// "5.0" compare equal to "5.0.0".
func normalizeVersion(v string) ([]int, error) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) > 3 {
		return nil, fmt.Errorf("version %q has more than three segments", v)
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("version %q is not numeric", v)
		}
		out[i] = n
	}
	return out, nil
}

func formatVersion(v []int) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// pessimisticCeiling returns the exclusive upper bound Terraform derives
// from "~> v": the second-to-last written segment is bumped. A single
// segment ("~> 5") has no upper bound, matching hashicorp/go-version.
func pessimisticCeiling(v string) (string, error) {
	segs := len(strings.Split(strings.TrimSpace(v), "."))
	n, err := normalizeVersion(v)
	if err != nil || segs == 1 {
		return "", err
	}
	c := make([]int, 3)
	copy(c, n[:segs-2])
	c[segs-2] = n[segs-2] + 1
	return "< " + formatVersion(c), nil
}

func splitConstraintOp(term string) (op, version string) {
	for _, candidate := range []string{">=", "<=", "~>", "!=", ">", "<", "="} {
		if strings.HasPrefix(term, candidate) {
			return candidate, strings.TrimSpace(strings.TrimPrefix(term, candidate))
		}
	}
	return "=", term
}

// applyConstraintTerm folds one comma-separated term of a constraint into b.
func applyConstraintTerm(b *versionBounds, term string) error {
	op, version := splitConstraintOp(strings.TrimSpace(term))
	n, err := normalizeVersion(version)
	if err != nil {
		return err
	}
	switch op {
	case ">=", "=":
		b.Floor = ">= " + formatVersion(n)
	case ">":
		b.Floor = "> " + formatVersion(n)
	case "<", "<=":
		b.Ceiling = op + " " + formatVersion(n)
	case "~>":
		b.Floor = ">= " + formatVersion(n)
		ceiling, cerr := pessimisticCeiling(version)
		if cerr != nil {
			return cerr
		}
		if ceiling != "" {
			b.Ceiling = ceiling
		}
	default:
		return fmt.Errorf("unsupported operator %q", op)
	}
	return nil
}

// constraintBounds reduces a Terraform version constraint to its bounds, so
// "~> 4.0" and ">= 4.0, < 5.0" compare equal.
func constraintBounds(constraint string) (versionBounds, error) {
	var b versionBounds
	for _, term := range strings.Split(constraint, ",") {
		if err := applyConstraintTerm(&b, term); err != nil {
			return versionBounds{}, fmt.Errorf("constraint %q: %w", constraint, err)
		}
	}
	return b, nil
}

// providerBoundsDisagreements returns one message per provider source whose
// floor or ceiling differs between bundles. providers maps a bundle name to
// its required_providers entries.
func providerBoundsDisagreements(providers map[string][]bundleProvider) ([]string, error) {
	seen := map[string]map[versionBounds][]string{} // source -> bounds -> bundles
	for bundle, entries := range providers {
		for _, p := range entries {
			b, err := constraintBounds(p.Version)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", bundle, p.Source, err)
			}
			if seen[p.Source] == nil {
				seen[p.Source] = map[versionBounds][]string{}
			}
			seen[p.Source][b] = append(seen[p.Source][b], bundle+" ("+p.Version+")")
		}
	}
	var out []string
	for source, byBounds := range seen {
		if len(byBounds) < 2 {
			continue
		}
		var groups []string
		for b, bundles := range byBounds {
			sort.Strings(bundles)
			groups = append(groups, fmt.Sprintf("%s: %s", b, strings.Join(bundles, ", ")))
		}
		sort.Strings(groups)
		out = append(out, fmt.Sprintf("%s has %d different bounds across bundles: %s",
			source, len(byBounds), strings.Join(groups, "; ")))
	}
	sort.Strings(out)
	return out, nil
}

// TestFederationBundles_ProviderBoundsAgreeAcrossBundles renders every
// customer federation bundle through the API and fails if two bundles declare
// a different floor or ceiling for the same provider (#55). A customer who
// downloads the AWS and GCP bundles, or onboards a second account later, must
// get one version policy per provider.
func TestFederationBundles_ProviderBoundsAgreeAcrossBundles(t *testing.T) {
	providers := map[string][]bundleProvider{}
	for _, c := range federationBundleCombos {
		files := renderBundleFiles(t, c.target, c.source, c.sourceCloud)
		mainTF, ok := files["terraform/main.tf"]
		require.True(t, ok, "%s bundle must contain terraform/main.tf", c.module)
		entries, err := parseRequiredProviders(mainTF)
		require.NoError(t, err, "%s: parse rendered terraform/main.tf", c.module)
		require.NotEmpty(t, entries, "%s: no required_providers entries", c.module)
		providers[c.module] = entries
	}

	disagreements, err := providerBoundsDisagreements(providers)
	require.NoError(t, err)
	assert.Empty(t, disagreements,
		"federation bundles must declare the same floor and ceiling for a shared provider")
}

// TestProviderBoundsDisagreements_DetectsDivergence proves the guard above can
// fail: a floor or ceiling that differs in one bundle is reported.
func TestProviderBoundsDisagreements_DetectsDivergence(t *testing.T) {
	aws := func(v string) bundleProvider { return bundleProvider{Name: "aws", Source: "hashicorp/aws", Version: v} }
	http := func(v string) bundleProvider {
		return bundleProvider{Name: "http", Source: "hashicorp/http", Version: v}
	}

	tests := []struct {
		name      string
		providers map[string][]bundleProvider
		want      []string // substrings, one per expected disagreement
	}{
		{
			name: "aligned bundles",
			providers: map[string][]bundleProvider{
				"a": {aws(">= 5.0, < 7.0"), http(">= 3.4")},
				"b": {aws(">= 5.0, < 7.0"), http(">= 3.4")},
			},
		},
		{
			name: "equivalent spellings are aligned",
			providers: map[string][]bundleProvider{
				"a": {http("~> 3.4")},
				"b": {http(">= 3.4.0, < 4")},
			},
		},
		{
			name: "different floor",
			providers: map[string][]bundleProvider{
				"a": {aws(">= 5.0")},
				"b": {aws(">= 6.0")},
			},
			want: []string{"hashicorp/aws has 2 different bounds"},
		},
		{
			name: "ceiling on only one bundle",
			providers: map[string][]bundleProvider{
				"a": {http(">= 3.4, < 4.0")},
				"b": {http(">= 3.4")},
				"c": {http(">= 3.4, < 4.0")},
			},
			want: []string{"hashicorp/http has 2 different bounds"},
		},
		{
			name: "provider in one bundle only is not compared",
			providers: map[string][]bundleProvider{
				"a": {aws(">= 5.0")},
				"b": {http(">= 3.4")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := providerBoundsDisagreements(tt.providers)
			require.NoError(t, err)
			require.Len(t, got, len(tt.want), "disagreements: %v", got)
			for i, w := range tt.want {
				assert.Contains(t, got[i], w)
			}
		})
	}
}

func TestConstraintBounds(t *testing.T) {
	tests := []struct {
		in   string
		want versionBounds
	}{
		{">= 5.0", versionBounds{Floor: ">= 5.0.0"}},
		{">= 5.0, < 7.0", versionBounds{Floor: ">= 5.0.0", Ceiling: "< 7.0.0"}},
		{"~> 4.0", versionBounds{Floor: ">= 4.0.0", Ceiling: "< 5.0.0"}},
		{"~> 3.8", versionBounds{Floor: ">= 3.8.0", Ceiling: "< 4.0.0"}},
		{"~> 1.2.3", versionBounds{Floor: ">= 1.2.3", Ceiling: "< 1.3.0"}},
		{"~> 5", versionBounds{Floor: ">= 5.0.0"}},
		{"5.1.0", versionBounds{Floor: ">= 5.1.0"}},
		{"> 2.0, <= 3.0", versionBounds{Floor: "> 2.0.0", Ceiling: "<= 3.0.0"}},
	}
	for _, tt := range tests {
		got, err := constraintBounds(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}

	for _, bad := range []string{">= 5.0-beta", "!= 5.0", ">= x", ""} {
		_, err := constraintBounds(bad)
		assert.Error(t, err, bad)
	}
}

func TestParseRequiredProviders(t *testing.T) {
	src := []byte(`terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      # comment
      source  = "hashicorp/aws"
      version = ">= 5.0, < 7.0"
    }

    http = {
      source  = "hashicorp/http"
      version = ">= 3.4"
    }
  }
}

resource "x" "y" {
  version = "ignored"
}
`)
	got, err := parseRequiredProviders(src)
	require.NoError(t, err)
	assert.Equal(t, []bundleProvider{
		{Name: "aws", Source: "hashicorp/aws", Version: ">= 5.0, < 7.0"},
		{Name: "http", Source: "hashicorp/http", Version: ">= 3.4"},
	}, got)

	bad := map[string]string{
		"no block":        "terraform {\n  required_version = \">= 1.5\"\n}\n",
		"unterminated":    "terraform {\n  required_providers {\n    aws = {\n      source = \"hashicorp/aws\"\n",
		"inline entry":    "required_providers {\n  aws = { source = \"hashicorp/aws\", version = \">= 5.0\" }\n}\n",
		"missing version": "required_providers {\n  aws = {\n    source = \"hashicorp/aws\"\n  }\n}\n",
		"unknown attr":    "required_providers {\n  aws = {\n    source = \"hashicorp/aws\"\n    version = \">= 5.0\"\n    configuration_aliases = [aws.x]\n  }\n}\n",
	}
	for name, s := range bad {
		_, err := parseRequiredProviders([]byte(s))
		assert.Error(t, err, name)
	}
}

// TestFederationBundleReadme_ListsProviderConstraints checks that every
// bundle's README.txt names each provider range its terraform/main.tf
// declares, and tells the customer how to move to a newer range (#55).
func TestFederationBundleReadme_ListsProviderConstraints(t *testing.T) {
	for _, c := range federationBundleCombos {
		t.Run(c.module, func(t *testing.T) {
			files := renderBundleFiles(t, c.target, c.source, c.sourceCloud)
			entries, err := parseRequiredProviders(files["terraform/main.tf"])
			require.NoError(t, err)
			readme := string(files["README.txt"])
			require.Contains(t, readme, "Provider versions")
			for _, p := range entries {
				assert.Regexp(t, `(?m)^  `+regexp.QuoteMeta(p.Source)+` +`+regexp.QuoteMeta(p.Version)+`$`, readme,
					"README must list %s %s", p.Source, p.Version)
			}
			assert.Contains(t, readme, "terraform init -upgrade")
			assert.Contains(t, readme, ".terraform.lock.hcl")
		})
	}
}
