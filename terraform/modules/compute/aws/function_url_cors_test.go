// Regression test for issue #310 (finding A02-015): the API serves PATCH
// (PATCH /api/plans/{id}, PATCH /api/ladder/tranches/{id}), but the Lambda
// Function URL CORS config in both the Terraform lambda module and the
// CloudFormation stack listed only GET, POST, PUT and DELETE. When a Function
// URL has a CORS config, AWS answers the preflight itself from that config,
// so a cross-origin PATCH was refused before it reached the handler, whatever
// the handler's own Access-Control-Allow-Methods said.
package aws_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// apiRoutedMethods are the verbs internal/api/router.go registers routes
// with. internal/api's TestHandleRequest_PreflightAllowsEveryRoutedMethod
// checks the handler's header against the router table itself.
var apiRoutedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

var (
	tfFunctionURLBlock   = regexp.MustCompile(`(?s)resource\s+"aws_lambda_function_url"\s+"main"\s*\{.*?\n\}`)
	tfCORSAllowMethods   = regexp.MustCompile(`(?m)^\s*allow_methods\s*=\s*\[([^\]]*)\]`)
	tfQuotedValue        = regexp.MustCompile(`"([^"]*)"`)
	cfnFunctionURLBlock  = regexp.MustCompile(`(?s)Type:\s*AWS::Lambda::Url\n.*?\n  \S`)
	cfnAllowMethodsList  = regexp.MustCompile(`(?m)^[ \t]*AllowMethods:[ \t]*\n((?:[ \t]*-[ \t].*\n)+)`)
	cfnAllowMethodsEntry = regexp.MustCompile(`(?m)^\s*-\s*(\S+)\s*$`)
)

func assertAllowsRoutedMethods(t *testing.T, site string, methods []string) {
	t.Helper()
	got := map[string]bool{}
	for _, m := range methods {
		got[m] = true
	}
	for _, m := range apiRoutedMethods {
		if !got[m] && !got["*"] {
			t.Errorf("%s: Function URL CORS allow-methods %v is missing %s, which the API routes; a cross-origin preflight for it is refused at the Function URL", site, methods, m)
		}
	}
}

func TestFunctionURLCORS_TerraformAllowsRoutedMethods(t *testing.T) {
	path := filepath.Join(repoRoot(t), "terraform", "modules", "compute", "aws", "lambda", "main.tf")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	block := tfFunctionURLBlock.FindString(string(data))
	if block == "" {
		t.Fatal(`lambda/main.tf has no resource "aws_lambda_function_url" "main" block`)
	}
	m := tfCORSAllowMethods.FindStringSubmatch(block)
	if m == nil {
		t.Fatal("aws_lambda_function_url.main has no cors allow_methods list")
	}
	values := tfQuotedValue.FindAllStringSubmatch(m[1], -1)
	methods := make([]string, 0, len(values))
	for _, v := range values {
		methods = append(methods, v[1])
	}
	assertAllowsRoutedMethods(t, "terraform lambda module", methods)
}

func TestFunctionURLCORS_CloudFormationAllowsRoutedMethods(t *testing.T) {
	path := filepath.Join(repoRoot(t), "cloudformation", "stacks", "CUDly", "template.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	block := cfnFunctionURLBlock.FindString(string(data))
	if block == "" {
		t.Fatal("CUDly template has no AWS::Lambda::Url resource")
	}
	m := cfnAllowMethodsList.FindStringSubmatch(block)
	if m == nil {
		t.Fatal("the AWS::Lambda::Url resource has no Cors AllowMethods list")
	}
	entries := cfnAllowMethodsEntry.FindAllStringSubmatch(m[1], -1)
	methods := make([]string, 0, len(entries))
	for _, v := range entries {
		methods = append(methods, v[1])
	}
	assertAllowsRoutedMethods(t, "CloudFormation CUDly stack", methods)
}
