package cicdpermissions_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rdsProxyGrantsNeeded = `enable_rds_proxy is no longer a literal false, so the RDS proxy can be created, and the deploy role cannot manage it (#157).
Decision A (#157): first run the real-AWS check that the default target group inherits the proxy's Project tag
(create a proxy with tags in staging, then rds list-tags-for-resource on its target-group ARN). If it is
untagged, go back to the owner; do not grant target-group:* without a gate. If tagged, add to the deploy policy
(policy_data.tf), both gated by StringEqualsIgnoreCase aws:ResourceTag/Project = CUDly:
  - db-proxy:*: rds:ModifyDBProxy, rds:DeleteDBProxy, rds:DeregisterDBProxyTargets, rds:AddTagsToResource,
    rds:RemoveTagsFromResource, and rds:ListTagsForResource
    (the provider reads tags on every refresh, proxy.go @Tags(identifierAttribute="arn"))
  - target-group:*: rds:ModifyDBProxyTargetGroup, rds:RegisterDBProxyTargets (also needs db:cudly-*),
    rds:DeregisterDBProxyTargets
  - rds:CreateDBProxy stays on "*" (tags travel inside the create call)
Then exercise it once in staging (create, no-op re-plan, destroy) and update this tripwire.`

// envTerraformFiles returns the comment-stripped text of every *.tf directly in
// terraform/environments/aws.
func envTerraformFiles(t *testing.T, envDir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(envDir, "*.tf"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing %s/*.tf: %v (%d files); the tripwire would pass vacuously", envDir, err, len(files))
	}
	out := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		var kept []string
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			kept = append(kept, line)
		}
		out[f] = strings.Join(kept, "\n")
	}
	return out
}

// trimTrailingComment drops a trailing # or // comment and surrounding space.
func trimTrailingComment(v string) string {
	if i := strings.IndexAny(v, "#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "//"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func TestTrimTrailingComment(t *testing.T) {
	for in, want := range map[string]string{
		" false":          "false",
		" false # off":    "false",
		"   false // off": "false",
		" true # false":   "true",
	} {
		if got := trimTrailingComment(in); got != want {
			t.Errorf("trimTrailingComment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRDSProxyStaysDisabledUntilTheDeployRoleCanManageIt fails when anything
// but one explicit literal false reaches modules/database/aws's
// enable_rds_proxy. The module default is true, so deleting the environment's
// line, or adding a second caller, would silently enable the proxy.
func TestRDSProxyStaysDisabledUntilTheDeployRoleCanManageIt(t *testing.T) {
	const envDir = "../"
	const moduleDir = "../../../modules/database/aws"

	assignment := regexp.MustCompile(`(?m)^\s*enable_rds_proxy\s*=(.*)$`)
	var values []string
	for _, text := range envTerraformFiles(t, envDir) {
		for _, m := range assignment.FindAllStringSubmatch(text, -1) {
			values = append(values, trimTrailingComment(m[1]))
		}
	}
	if len(values) != 1 || values[0] != "false" {
		t.Fatalf("want exactly one uncommented `enable_rds_proxy = false` in %s*.tf, got %q.\n%s", envDir, values, rdsProxyGrantsNeeded)
	}

	// Single caller of the module: the env's explicit false is the only thing
	// overriding the module default of true.
	source := regexp.MustCompile(`(?m)^\s*source\s*=\s*"[^"]*modules/database/aws"`)
	moduleAbs, _ := filepath.Abs(moduleDir)
	callers := 0
	root := "../../../"
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		abs, _ := filepath.Abs(p)
		if d.IsDir() && (abs == moduleAbs || d.Name() == ".terraform") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".tf") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		callers += len(source.FindAllString(stripCommentLines(string(data)), -1))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if callers != 1 {
		t.Fatalf("modules/database/aws has %d callers, want exactly 1 (the env passing enable_rds_proxy = false; the module default is true).\n%s", callers, rdsProxyGrantsNeeded)
	}
}
