// Regression test for issue #324 (audit finding A13c-024): the Cloud Run
// module's OIDC signing key had an explicit `prevent_destroy = false` and no
// create_before_destroy, so a change forcing a new key destroyed the live
// signing key first (scheduling its versions for destruction stops
// AsymmetricSign at once) and broke client-assertion JWT minting. Its AWS twin
// in modules/compute/aws/lambda/signing-key.tf already creates first (cli#1480).
// terraform test cannot observe lifecycle settings, so this reads the source;
// the retention input is covered by cloud-run/tests/signing_key.tftest.hcl.
package gcp_test

import (
	"regexp"
	"testing"
)

func TestGCPSigningKeyCreatesBeforeDestroy(t *testing.T) {
	src := codeLines(readTF(t, "cloud-run/signing-key.tf"))
	key := probeBlock(t, src, `resource\s+"google_kms_crypto_key"\s+"signing"`)
	lifecycle := probeBlock(t, key, `lifecycle`)

	if !regexp.MustCompile(`(?m)^\s*create_before_destroy\s*=\s*true\s*$`).MatchString(lifecycle) {
		t.Errorf("google_kms_crypto_key.signing must set lifecycle { create_before_destroy = true }, got:\n%s", lifecycle)
	}
	if regexp.MustCompile(`(?m)^\s*prevent_destroy\s*=\s*false\s*$`).MatchString(lifecycle) {
		t.Error("google_kms_crypto_key.signing restates prevent_destroy = false, Terraform's default; drop it (see the comment in signing-key.tf)")
	}
	if !regexp.MustCompile(`(?m)^\s*destroy_scheduled_duration\s*=\s*var\.signing_key_destroy_scheduled_duration\s*$`).MatchString(key) {
		t.Error("google_kms_crypto_key.signing must take destroy_scheduled_duration from var.signing_key_destroy_scheduled_duration")
	}
}

// TestAWSSigningKeyCreatesBeforeDestroy pins the twin the GCP key is matched
// against, so the two cannot drift apart again from the other side.
func TestAWSSigningKeyCreatesBeforeDestroy(t *testing.T) {
	src := codeLines(readTF(t, "../aws/lambda/signing-key.tf"))
	key := probeBlock(t, src, `resource\s+"aws_kms_key"\s+"signing"`)
	lifecycle := probeBlock(t, key, `lifecycle`)
	if !regexp.MustCompile(`(?m)^\s*create_before_destroy\s*=\s*true\s*$`).MatchString(lifecycle) {
		t.Errorf("aws_kms_key.signing must keep lifecycle { create_before_destroy = true }, got:\n%s", lifecycle)
	}
}
