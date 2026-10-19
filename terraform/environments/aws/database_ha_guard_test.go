// Regression test for issue #719: the AWS prod database was a single-AZ
// burstable db.t4g.micro and the database module could not express Multi-AZ,
// while GCP (REGIONAL) and Azure (ZoneRedundant) prod were already HA. The
// module half is covered by terraform/modules/database/aws/tests; this pins
// the environment wiring.
package aws_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestAWSDatabaseMultiAZIsWired(t *testing.T) {
	cases := []struct {
		file string
		re   *regexp.Regexp
		what string
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
			file: filepath.Join("..", "..", "modules", "database", "aws", "main.tf"),
			re:   regexp.MustCompile(`resource\s+"aws_db_instance"\s+"main"\s*\{[\s\S]*?\n\s*multi_az\s*=\s*var\.multi_az\s*\n`),
			what: `multi_az = var.multi_az on aws_db_instance.main`,
		},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("reading %s: %v", c.file, err)
		}
		if !c.re.Match(data) {
			t.Errorf("%s: missing %s", c.file, c.what)
		}
	}
}
