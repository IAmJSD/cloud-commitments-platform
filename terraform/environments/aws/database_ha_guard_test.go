// Regression test for issue #719: the AWS prod database was a single-AZ
// burstable db.t4g.micro and the database module could not express Multi-AZ,
// while GCP (REGIONAL) and Azure (ZoneRedundant) prod were already HA. The
// module half is covered by terraform/modules/database/aws/tests; this pins
// the environment wiring and the prod values.
package aws_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tfvarsValue returns the right-hand side of a top-level `name = value`
// assignment in a tfvars file, ignoring comment lines and trailing comments.
func tfvarsValue(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `\s*=\s*(.*)$`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if m := re.FindStringSubmatch(line); m != nil {
			return strings.Trim(strings.TrimSpace(m[1]), `"`), true
		}
	}
	return "", false
}

func TestAWSProdDatabaseIsHighlyAvailable(t *testing.T) {
	const prod = "github-prod.tfvars"

	if v, ok := tfvarsValue(t, prod, "database_multi_az"); !ok || v != "true" {
		t.Errorf("%s: database_multi_az = %q (set: %v), want true: prod RDS needs a standby in a second AZ", prod, v, ok)
	}

	class, ok := tfvarsValue(t, prod, "database_instance_class")
	if !ok {
		t.Fatalf("%s: database_instance_class not set, so prod falls back to the burstable db.t4g.micro default", prod)
	}
	if regexp.MustCompile(`^db\.t\d`).MatchString(class) {
		t.Errorf("%s: database_instance_class = %q is burstable (CPU credits); prod needs a non-burstable class", prod, class)
	}
}

// topLevelBlock returns the text of the top-level block that starts with
// header, up to its closing brace in column 0 (terraform fmt layout), or ""
// when the header is absent.
func topLevelBlock(data []byte, header *regexp.Regexp) string {
	loc := header.FindIndex(data)
	if loc == nil {
		return ""
	}
	rest := string(data[loc[0]:])
	if end := strings.Index(rest, "\n}"); end >= 0 {
		return rest[:end+2]
	}
	return rest
}

func TestAWSDatabaseMultiAZIsWired(t *testing.T) {
	cases := []struct {
		file  string
		block *regexp.Regexp // when set, re must match inside this top-level block
		re    *regexp.Regexp
		what  string
	}{
		{
			file: "variables.tf",
			re:   regexp.MustCompile(`variable\s+"database_multi_az"\s*\{[^}]*type\s*=\s*bool[^}]*default\s*=\s*false`),
			what: `variable "database_multi_az" (bool, default false)`,
		},
		{
			file: "database.tf",
			re:   regexp.MustCompile(`(?m)^\s*multi_az\s*=\s*var\.database_multi_az\s*$`),
			what: "multi_az = var.database_multi_az on module \"database\"",
		},
		{
			file:  filepath.Join("..", "..", "modules", "database", "aws", "main.tf"),
			block: regexp.MustCompile(`(?m)^resource\s+"aws_db_instance"\s+"main"\s*\{`),
			re:    regexp.MustCompile(`(?m)^\s*multi_az\s*=\s*var\.multi_az\s*$`),
			what:  `multi_az = var.multi_az on aws_db_instance.main`,
		},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("reading %s: %v", c.file, err)
		}
		if c.block != nil {
			data = []byte(topLevelBlock(data, c.block))
		}
		if !c.re.Match(data) {
			t.Errorf("%s: missing %s", c.file, c.what)
		}
	}
}
