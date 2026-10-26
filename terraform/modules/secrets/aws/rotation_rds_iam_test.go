// Package aws_test guards the RDS grant of the secret-rotation Lambda role.
//
// Regression test for issue #323 (A13c-021): the rotation role's RDS
// statement used Resource "arn:aws:rds:${var.region}:*:db:*", which let it
// call rds:ModifyDBInstance on every DB instance in the account, although the
// policy's own count already requires the one instance it serves
// (var.rds_cluster_id). The grant must name that instance in this account and
// still carry the two actions rotation needs.
package aws_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const wantRotationRDSResource = `"arn:aws:rds:${var.region}:${data.aws_caller_identity.current.account_id}:db:${var.rds_cluster_id}"`

var (
	rotationRDSBlockRe  = regexp.MustCompile(`(?s)resource\s+"aws_iam_role_policy"\s+"rotation_rds"\s*\{(.*?)\n\}`)
	resourceLineRe      = regexp.MustCompile(`(?m)^\s*Resource\s*=\s*(.+?)\s*$`)
	actionListRe        = regexp.MustCompile(`(?s)Action\s*=\s*\[(.*?)\]`)
	quotedRe            = regexp.MustCompile(`"([^"]*)"`)
	callerIdentityRe    = regexp.MustCompile(`(?m)^data\s+"aws_caller_identity"\s+"current"\s*\{\s*\}`)
	rdsClusterIDBlockRe = regexp.MustCompile(`(?s)variable\s+"rds_cluster_id"\s*\{(.*?)\n\}`)
	validationRegexRe   = regexp.MustCompile(`can\(regex\("((?:[^"\\]|\\.)*)",\s*var\.rds_cluster_id\)\)`)
)

// codeLines drops whole-line comments so prose naming the old ARN is not
// mistaken for the grant.
func codeLines(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func readCode(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return codeLines(string(data))
}

func TestRotationRDSGrantNamesTheTargetInstance(t *testing.T) {
	code := readCode(t, "main.tf")

	block := rotationRDSBlockRe.FindStringSubmatch(code)
	if block == nil {
		t.Fatal(`main.tf must define resource "aws_iam_role_policy" "rotation_rds"`)
	}

	resources := resourceLineRe.FindAllStringSubmatch(block[1], -1)
	if len(resources) != 1 {
		t.Fatalf("rotation_rds: want exactly one Resource line, got %d", len(resources))
	}
	if got := resources[0][1]; got != wantRotationRDSResource {
		t.Errorf("rotation_rds Resource = %s, want %s (issue #323: no account or instance wildcard)", got, wantRotationRDSResource)
	}

	// The rotation function still needs both actions on that instance.
	actions := actionListRe.FindStringSubmatch(block[1])
	if actions == nil {
		t.Fatal("rotation_rds: no Action list")
	}
	quoted := quotedRe.FindAllStringSubmatch(actions[1], -1)
	got := make([]string, 0, len(quoted))
	for _, q := range quoted {
		got = append(got, q[1])
	}
	slices.Sort(got)
	if want := []string{"rds:DescribeDBInstances", "rds:ModifyDBInstance"}; !slices.Equal(got, want) {
		t.Errorf("rotation_rds actions = %v, want %v", got, want)
	}

	// The ARN's account segment comes from the caller identity, so the data
	// source must exist in the module.
	if !callerIdentityRe.MatchString(code) {
		t.Error(`main.tf must declare data "aws_caller_identity" "current" {} for the rotation RDS ARN`)
	}

	// No RDS ARN anywhere in the module may wildcard the account segment.
	if strings.Contains(code, "arn:aws:rds:${var.region}:*:") {
		t.Error("main.tf still has an RDS ARN with a wildcard account segment")
	}
}

// The instance id is interpolated into the ARN, so an input such as "*" or
// "cudly-*" would re-widen the grant. variables.tf must reject them.
func TestRotationRDSClusterIDRejectsWildcards(t *testing.T) {
	code := readCode(t, "variables.tf")

	block := rdsClusterIDBlockRe.FindStringSubmatch(code)
	if block == nil {
		t.Fatal(`variables.tf must define variable "rds_cluster_id"`)
	}
	if !strings.Contains(block[1], "var.rds_cluster_id == null ||") {
		t.Error("rds_cluster_id validation must keep null valid (rotation disabled)")
	}
	m := validationRegexRe.FindStringSubmatch(block[1])
	if m == nil {
		t.Fatal("rds_cluster_id must be validated with can(regex(..., var.rds_cluster_id))")
	}
	// Terraform's regex() and Go's regexp both use RE2 syntax; the HCL string
	// escapes are the only translation needed.
	pattern := strings.ReplaceAll(m[1], `\\`, `\`)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("rds_cluster_id validation regex %q does not compile: %v", pattern, err)
	}

	for _, ok := range []string{"cudly-dev-postgres", "a", "db1"} {
		if !re.MatchString(ok) {
			t.Errorf("rds_cluster_id validation rejects valid identifier %q", ok)
		}
	}
	for _, bad := range []string{"*", "cudly-*", "db?", "a:b", "a/b", "1db", "", "-db", strings.Repeat("a", 64)} {
		if re.MatchString(bad) {
			t.Errorf("rds_cluster_id validation accepts %q, which would widen or break the ARN", bad)
		}
	}
}
