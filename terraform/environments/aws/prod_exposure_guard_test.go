// Regression test for issue #128 (AWS half): prod's Lambda Function URL is
// unauthenticated (lambda_function_url_auth_type derives from enable_cdn,
// compute.tf) with no network-layer gate whenever enable_cdn = false. This
// mirrors terraform/environments/gcp/checks.tf's identical guard for the
// GCP half of the same defect.
package aws_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAWSConfigurationGuardPresent(t *testing.T) {
	data, err := os.ReadFile("checks.tf")
	if err != nil {
		t.Fatalf("reading checks.tf: %v", err)
	}
	checks := string(data)

	if !strings.Contains(checks, `check "prod_requires_network_authenticated_ingress"`) {
		t.Fatal(`checks.tf missing check "prod_requires_network_authenticated_ingress" (#128 continuous-validation guard)`)
	}
	if !regexp.MustCompile(`condition\s*=\s*!\(var\.environment\s*==\s*"prod"\s*&&\s*!var\.enable_cdn\)`).MatchString(checks) {
		t.Error(`checks.tf: prod_requires_network_authenticated_ingress must assert !(var.environment == "prod" && !var.enable_cdn)`)
	}
}

// Regression test for issue #412: with compute_platform = "fargate" and
// enable_cdn = true, CloudFront fronts the ALB but the ALB stayed open to the
// internet, so clients could bypass the distribution. networking.tf must
// restrict the ALB to CloudFront's origin-facing prefix list in exactly that
// case, and checks.tf must warn whenever the ALB is not so restricted.
// tests/fargate_cdn_alb.tftest.hcl exercises the planned outcome; this test
// pins the exact wiring and condition, which a mock plan cannot drive into the
// failing state.
func TestAWSFargateCDNGuardPresent(t *testing.T) {
	checks, err := os.ReadFile("checks.tf")
	if err != nil {
		t.Fatalf("reading checks.tf: %v", err)
	}
	if !strings.Contains(string(checks), `check "fargate_cdn_requires_cloudfront_only_alb"`) {
		t.Fatal(`checks.tf missing check "fargate_cdn_requires_cloudfront_only_alb" (#412)`)
	}
	cond := regexp.MustCompile(`condition\s*=\s*!\(var\.compute_platform\s*==\s*"fargate"\s*&&\s*var\.enable_cdn\)\s*\|\|\s*module\.networking\.alb_ingress_cloudfront_only\s*\n`)
	if !cond.Match(checks) {
		t.Error(`checks.tf: fargate_cdn_requires_cloudfront_only_alb must assert !(var.compute_platform == "fargate" && var.enable_cdn) || module.networking.alb_ingress_cloudfront_only`)
	}

	networking, err := os.ReadFile("networking.tf")
	if err != nil {
		t.Fatalf("reading networking.tf: %v", err)
	}
	wiring := regexp.MustCompile(`(?m)^\s*restrict_alb_ingress_to_cloudfront\s*=\s*var\.compute_platform\s*==\s*"fargate"\s*&&\s*var\.enable_cdn\s*$`)
	if !wiring.Match(networking) {
		t.Error(`networking.tf: module "networking" must set restrict_alb_ingress_to_cloudfront = var.compute_platform == "fargate" && var.enable_cdn`)
	}
}
