package cicdpermissions_test

// Guard for #309: the policy_arns output describes itself as the ARNs of all
// managed policies attached to the deploy role, and operators enumerate it
// (`terraform output -json policy_arns`) to review or detach them. It was a
// hand-maintained map that drifted: compute_b and iam were attached in role.tf
// but never listed, so an audit or teardown driven by the output skipped the
// two policies carrying the #1705 boundary-delegation half of the role. The
// test below re-derives the attachment set from source and requires the
// output to match it exactly, in both directions.

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const outputsFile = "outputs.tf"

// minDeployRoleAttachments is the number of aws_iam_role_policy_attachment
// resources that existed when this guard was written. It keeps the source
// scan from silently degenerating into an empty (trivially passing)
// comparison if the attachments move or change shape.
const minDeployRoleAttachments = 6

var (
	// attachmentResourcePattern matches the header of one attachment resource
	// and captures its resource name.
	attachmentResourcePattern = regexp.MustCompile(`resource\s+"aws_iam_role_policy_attachment"\s+"([^"]+)"\s*\{`)

	// policyARNAttrPattern matches the policy_arn argument inside one
	// attachment resource and captures its expression.
	policyARNAttrPattern = regexp.MustCompile(`(?m)^\s*policy_arn\s*=\s*(\S+)\s*$`)

	// policyARNsOutputPattern matches the header of the policy_arns output.
	policyARNsOutputPattern = regexp.MustCompile(`output\s+"policy_arns"\s*\{`)

	// mapValuePattern matches the `value = {` that opens the output's map.
	mapValuePattern = regexp.MustCompile(`value\s*=\s*\{`)

	// mapEntryPattern matches one `key = expression` entry of that map.
	mapEntryPattern = regexp.MustCompile(`^([A-Za-z0-9_-]+)\s*=\s*(\S+)$`)
)

// deployRoleAttachments returns every aws_iam_role_policy_attachment declared
// in this root, keyed by resource name, mapped to its policy_arn expression.
// It scans every *.tf file rather than only role.tf so an attachment added in
// another file is covered too.
func deployRoleAttachments(t *testing.T) map[string]string {
	t.Helper()

	files, err := filepath.Glob("*.tf")
	if err != nil {
		t.Fatalf("globbing *.tf: %v", err)
	}

	attachments := map[string]string{}
	for _, f := range files {
		content := readPolicySource(t, f)
		for _, loc := range attachmentResourcePattern.FindAllStringSubmatchIndex(content, -1) {
			name := content[loc[2]:loc[3]]
			body := content[loc[0]:balancedBraceEnd(content, loc[1]-1)]
			m := policyARNAttrPattern.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("%s: aws_iam_role_policy_attachment.%s has no single-line policy_arn argument this guard can read; extend outputs_guard_test.go before changing its shape", f, name)
			}
			if _, dup := attachments[name]; dup {
				t.Fatalf("aws_iam_role_policy_attachment.%s is declared more than once", name)
			}
			attachments[name] = m[1]
		}
	}

	if len(attachments) < minDeployRoleAttachments {
		t.Fatalf("found only %d aws_iam_role_policy_attachment resources (%v), fewer than the %d that existed when this guard was written; the scan no longer sees them", len(attachments), sortedKeysOf(attachments), minDeployRoleAttachments)
	}
	return attachments
}

// policyARNsOutputEntries returns the literal map entries of the policy_arns
// output in outputs.tf, keyed by map key, mapped to their value expression.
func policyARNsOutputEntries(t *testing.T) map[string]string {
	t.Helper()

	content := readPolicySource(t, outputsFile)
	loc := policyARNsOutputPattern.FindStringIndex(content)
	if loc == nil {
		t.Fatalf("%s: no output \"policy_arns\" block", outputsFile)
	}
	block := content[loc[0]:balancedBraceEnd(content, loc[1]-1)]

	vloc := mapValuePattern.FindStringIndex(block)
	if vloc == nil {
		t.Fatalf("%s: policy_arns has no literal `value = { ... }` map this guard can read", outputsFile)
	}
	inner := block[vloc[1] : balancedBraceEnd(block, vloc[1]-1)-1]

	entries := map[string]string{}
	for _, line := range strings.Split(inner, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := mapEntryPattern.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: policy_arns entry %q is not a plain `key = expression` line this guard can read", outputsFile, line)
		}
		if _, dup := entries[m[1]]; dup {
			t.Fatalf("%s: policy_arns lists key %q more than once", outputsFile, m[1])
		}
		entries[m[1]] = m[2]
	}
	return entries
}

func sortedKeysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestPolicyARNsOutputListsEveryAttachedPolicy requires policy_arns to hold
// exactly one entry per aws_iam_role_policy_attachment, keyed by the
// attachment's resource name and pointing at the same policy ARN expression
// the attachment uses. Missing entries are the #309 bug; extra entries would
// list a policy that is not attached to the role, which the output's
// description rules out.
func TestPolicyARNsOutputListsEveryAttachedPolicy(t *testing.T) {
	attachments := deployRoleAttachments(t)
	entries := policyARNsOutputEntries(t)

	for _, name := range sortedKeysOf(attachments) {
		want := attachments[name]
		got, ok := entries[name]
		if !ok {
			t.Errorf("aws_iam_role_policy_attachment.%s attaches %s to the deploy role, but %s's policy_arns output has no %q entry; operators enumerating the output would miss it (#309)", name, want, outputsFile, name)
			continue
		}
		if got != want {
			t.Errorf("policy_arns[%q] = %s, but aws_iam_role_policy_attachment.%s attaches %s", name, got, name, want)
		}
	}

	for _, key := range sortedKeysOf(entries) {
		if _, ok := attachments[key]; !ok {
			t.Errorf("policy_arns lists %q = %s, but no aws_iam_role_policy_attachment.%s attaches it to the deploy role (attachments: %v)", key, entries[key], key, sortedKeysOf(attachments))
		}
	}
}
