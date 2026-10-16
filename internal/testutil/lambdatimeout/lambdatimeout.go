// Package lambdatimeout pins the AWS Lambda timeouts the repository ships
// against the Go budgets for work detached from the request (#913). It is a
// separate package from testutil so packages that testutil imports (such as
// purchase) can use it without an import cycle.
package lambdatimeout

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// Margin is the time an AWS Lambda invocation needs on top of a
// detached budget for the request work that runs before the detach (auth,
// database reads, quote lookups) and for writing the response.
const Margin = 30 * time.Second

var (
	tfModuleTimeoutVar  = regexp.MustCompile(`(?s)variable "timeout" \{.*?\n\}`)
	tfRootTimeoutVar    = regexp.MustCompile(`(?s)variable "lambda_timeout" \{.*?\n\}`)
	tfDefault           = regexp.MustCompile(`(?m)^\s*default\s*=\s*(\d+)`)
	tfModuleFloor       = regexp.MustCompile(`var\.timeout\s*>=\s*(\d+)`)
	tfRootFloor         = regexp.MustCompile(`var\.lambda_timeout\s*>=\s*(\d+)`)
	tfvarsLambdaTimeout = regexp.MustCompile(`(?m)^\s*lambda_timeout\s*=\s*(\d+)`)
	cfnLambdaTimeout    = regexp.MustCompile(`(?s)LambdaFunction:\s*\n\s*Type: AWS::Lambda::Function\b.*?\n\s{6}Timeout:\s*(\d+)`)
)

// AssertCovers fails t unless every AWS Lambda timeout the
// repository ships covers budget plus Margin: the Terraform
// module and root variable defaults and their validation floors, every
// committed lambda_timeout tfvars value (including examples and the
// deployment guide), and the CloudFormation function's Timeout. Lambda ends an
// invocation at its timeout whatever a context deadline says, so detached
// work with a longer budget would be cut off mid-flight (#913).
//
// repoRoot is the repository root relative to the calling test's package.
func AssertCovers(t *testing.T, repoRoot string, budget time.Duration) {
	t.Helper()
	need := budget + Margin

	check := func(source string, secs int) {
		t.Helper()
		if got := time.Duration(secs) * time.Second; got < need {
			t.Errorf("%s: Lambda timeout %v is shorter than the %v detached budget plus %v margin (want >= %v)",
				source, got, budget, Margin, need)
		}
	}
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repoRoot, rel)) // #nosec G304 -- fixed repo-relative paths
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		return string(data)
	}
	one := func(source string, re *regexp.Regexp, text string) {
		t.Helper()
		m := re.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("%s: not found (pattern %s); keep this check in sync with the IaC", source, re)
			return
		}
		check(source, atoi(t, m[1]))
	}

	for _, v := range []struct {
		file  string
		block *regexp.Regexp
		floor *regexp.Regexp
	}{
		{"terraform/modules/compute/aws/lambda/variables.tf", tfModuleTimeoutVar, tfModuleFloor},
		{"terraform/environments/aws/variables.tf", tfRootTimeoutVar, tfRootFloor},
	} {
		block := v.block.FindString(read(v.file))
		if block == "" {
			t.Errorf("%s: Lambda timeout variable not found", v.file)
			continue
		}
		one(v.file+" default", tfDefault, block)
		one(v.file+" validation floor", v.floor, block)
	}

	files := []string{"docs/DEPLOYMENT.md"}
	for _, pattern := range []string{"terraform/environments/aws/*.tfvars*", "terraform/profiles/aws/*.tfvars*"} {
		matches, err := filepath.Glob(filepath.Join(repoRoot, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, m := range matches {
			rel, relErr := filepath.Rel(repoRoot, m)
			if relErr != nil {
				t.Fatalf("relative path for %s: %v", m, relErr)
			}
			files = append(files, rel)
		}
	}
	values := 0
	for _, f := range files {
		for _, m := range tfvarsLambdaTimeout.FindAllStringSubmatch(read(f), -1) {
			check(f+" lambda_timeout", atoi(t, m[1]))
			values++
		}
	}
	if values < 4 {
		t.Errorf("found %d lambda_timeout settings in tfvars/docs, want at least 4; keep this check in sync with the IaC", values)
	}

	one("cloudformation/stacks/CUDly/template.yaml LambdaFunction Timeout", cfnLambdaTimeout,
		read("cloudformation/stacks/CUDly/template.yaml"))
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(fmt.Errorf("parsing %q: %w", s, err))
	}
	return n
}
