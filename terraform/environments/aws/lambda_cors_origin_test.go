// Regression tests for issue #324 (audit finding A13c-017): the dev Lambda
// Function URL's hostname was pasted into github-dev.tfvars by hand, so a
// redeploy that reissued the URL left the CORS allowlist pointing at a dead
// origin with no Terraform diff. The origin now comes from the previous
// apply's lambda_function_url output, read by
// scripts/read-lambda-function-url.sh and passed as
// TF_VAR_lambda_deployed_function_url. The merge itself is exercised by
// tests/lambda_cors_origins.tftest.hcl; these tests pin the wiring around it.
package aws_test

import (
	"regexp"
	"strings"
	"testing"
)

var functionURLHostRe = regexp.MustCompile(`[a-z0-9]+\.lambda-url\.[a-z0-9-]+\.on\.aws`)

func TestTfvarsCarryNoFunctionURLHost(t *testing.T) {
	for _, env := range []string{"dev", "staging", "prod"} {
		path := "github-" + env + ".tfvars"
		src := readFile(t, path)
		if host := functionURLHostRe.FindString(src); host != "" {
			t.Errorf("%s pins the server-assigned Function URL host %s; the deploy workflow supplies it from state (#324)", path, host)
		}
		if regexp.MustCompile(`(?m)^\s*lambda_deployed_function_url\s*=`).MatchString(src) {
			t.Errorf("%s sets lambda_deployed_function_url; a -var-file value would override the workflow's TF_VAR_ (#324)", path)
		}
	}
}

func TestLambdaModuleGetsMergedCORSOrigins(t *testing.T) {
	src := readFile(t, "compute.tf")
	start := strings.Index(src, `module "compute_lambda" {`)
	if start < 0 {
		t.Fatal(`compute.tf has no module "compute_lambda"`)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal(`unterminated module "compute_lambda" block`)
	}
	if !regexp.MustCompile(`(?m)^\s*allowed_origins\s*=\s*local\.lambda_cors_origins\s*$`).MatchString(src[start : start+end]) {
		t.Error("module.compute_lambda must take allowed_origins from local.lambda_cors_origins, which adds the deployed Function URL's origin (#324)")
	}
}

// workflowStep returns the text of the step named name in a workflow file,
// from its "- name:" line up to the next step or job.
func workflowStep(t *testing.T, src, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^(\s*)- name: ` + regexp.QuoteMeta(name) + `\s*$`).FindStringSubmatchIndex(src)
	if loc == nil {
		t.Fatalf("no step named %q", name)
	}
	indent := src[loc[2]:loc[3]]
	rest := src[loc[1]:]
	next := regexp.MustCompile(`(?m)^(` + indent + `- name:|  [A-Za-z0-9_-]+:\s*$)`).FindStringIndex(rest)
	if next == nil {
		return src[loc[0]:]
	}
	return src[loc[0] : loc[1]+next[0]]
}

func TestLambdaDeployPassesDeployedFunctionURL(t *testing.T) {
	src := readFile(t, "../../../.github/workflows/deploy-aws-lambda.yml")

	read := workflowStep(t, src, "Read deployed Function URL")
	if !strings.Contains(read, "scripts/read-lambda-function-url.sh terraform/environments/aws") {
		t.Errorf("the read step must use scripts/read-lambda-function-url.sh, got:\n%s", read)
	}
	plan := workflowStep(t, src, "Terraform Plan")
	if !strings.Contains(plan, "TF_VAR_lambda_deployed_function_url: ${{ steps.deployed-url.outputs.url }}") {
		t.Errorf("Terraform Plan must pass the deployed Function URL as TF_VAR_lambda_deployed_function_url, got:\n%s", plan)
	}
	if strings.Index(src, "- name: Read deployed Function URL") > strings.Index(src, "- name: Terraform Plan") {
		t.Error("the Function URL must be read before Terraform Plan")
	}
}

func TestLambdaRollbackPassesDeployedFunctionURL(t *testing.T) {
	src := readFile(t, "../../../.github/workflows/rollback.yml")
	job := src[strings.Index(src, "\n  rollback-aws-lambda:"):]
	job = job[:strings.Index(job[1:], "\n  rollback-")+1]
	step := workflowStep(t, job, "Rollback with Terraform")

	read := strings.Index(step, "TF_VAR_lambda_deployed_function_url=$(../../../scripts/read-lambda-function-url.sh .)")
	export := strings.Index(step, "export TF_VAR_lambda_deployed_function_url")
	apply := strings.Index(step, "terraform apply")
	if read < 0 || export < 0 || apply < 0 || read > apply || export > apply {
		t.Errorf("the Lambda rollback must read and export TF_VAR_lambda_deployed_function_url before terraform apply, or it drops the origin from CORS; got:\n%s", step)
	}
}
