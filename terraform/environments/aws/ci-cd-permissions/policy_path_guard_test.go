package cicdpermissions_test

// Guards for #197: the cudly-* name-prefix scopes in these policies are
// trailing-wildcard ARNs (arn:aws:iam::*:role/cudly-*), and in an IAM resource
// ARN `*` matches any run of characters INCLUDING `/`. The IAM path is part of
// the ARN and is chosen by whoever creates the resource, so a role created at
// Path "/cudly-x/" with any name (ARN .../role/cudly-x/EvilRole) satisfies the
// prefix without its NAME carrying it. The fix is an unconditioned Deny on the
// path-bearing shape cudly-*/* in each document, which is exact only while
// every genuine cudly-* resource sits at the root path; the tests below pin
// both halves, and both directions of the Deny (impostor refused, genuine
// resources untouched), so a fix that refuses everything cannot pass.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// iamARNMatches reports whether arn satisfies the IAM Resource pattern, using
// the semantics IAM applies to resource ARNs: `*` matches any run of
// characters, `/` included, and `?` matches exactly one character. The match
// is anchored at both ends and case-sensitive, as resource ARN matching is.
func iamARNMatches(pattern, arn string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(arn)
}

// TestIAMARNMatchesCrossesPathSeparator pins the matcher the guards below
// delegate to, including the property #197 is about. A matcher whose `*` stops
// at `/` would report the bypass as impossible and every guard here as
// vacuously satisfied.
func TestIAMARNMatchesCrossesPathSeparator(t *testing.T) {
	cases := []struct {
		pattern, arn string
		want         bool
	}{
		// The #197 bypass: the prefix is carried in the path.
		{"arn:aws:iam::*:role/cudly-*", "arn:aws:iam::123456789012:role/cudly-x/EvilRole", true},
		// A prefix in the NAME under a foreign path never matched.
		{"arn:aws:iam::*:role/cudly-*", "arn:aws:iam::123456789012:role/evil/cudly-x", false},
		{"arn:aws:iam::*:role/cudly-*", "arn:aws:iam::123456789012:role/cudly-dev-lambda", true},
		// The Deny shape: path-bearing only, never a root-path name.
		{"arn:aws:iam::*:role/cudly-*/*", "arn:aws:iam::123456789012:role/cudly-x/EvilRole", true},
		{"arn:aws:iam::*:role/cudly-*/*", "arn:aws:iam::123456789012:role/cudly-a/b/EvilRole", true},
		{"arn:aws:iam::*:role/cudly-*/*", "arn:aws:iam::123456789012:role/cudly-dev-lambda", false},
		{"arn:aws:iam::*:role/cudly-*/*", "arn:aws:iam::123456789012:role/evil/cudly-x", false},
		// Anchored and case-sensitive.
		{"arn:aws:iam::*:role/cudly-*", "xarn:aws:iam::123456789012:role/cudly-a", false},
		{"arn:aws:iam::*:role/cudly-*", "arn:aws:iam::123456789012:role/CUDLY-a", false},
		{"arn:aws:iam::*:role/cudly-?", "arn:aws:iam::123456789012:role/cudly-a", true},
		{"arn:aws:iam::*:role/cudly-?", "arn:aws:iam::123456789012:role/cudly-ab", false},
	}
	for _, c := range cases {
		if got := iamARNMatches(c.pattern, c.arn); got != c.want {
			t.Errorf("iamARNMatches(%q, %q) = %v, want %v", c.pattern, c.arn, got, c.want)
		}
	}
}

// deployPolicyFiles returns every document attached to cudly-terraform-deploy:
// the guarded policy_*.tf documents minus the boundary (which caps the
// workload roles, not the deploy role), plus policy_iam.tf.
func deployPolicyFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, f := range guardedPolicyFiles(t) {
		if filepath.Base(f) != boundaryFile {
			files = append(files, f)
		}
	}
	return append(files, iamFile)
}

// isIAMAction reports whether a statement's action can apply to an IAM
// resource ARN at all.
func isIAMAction(action string) bool {
	return action == "*" || strings.HasPrefix(strings.ToLower(action), "iam:")
}

// pathGuardCase is one ARN fed to assertPathBearingARNsDenied.
type pathGuardCase struct {
	arn string
	// requireGrant: the documents must contain a cudly- prefix-scoped Allow
	// matching arn, so the guard is not vacuous (for an impostor: the bypass
	// exists absent the Deny; for a genuine resource: the fix did not refuse
	// everything).
	requireGrant bool
}

// assertPathBearingARNsDenied checks, across the statements of files, that:
//
//   - every IAM action an Allow grants to an impostor ARN through a cudly-
//     prefix-scoped Resource is matched by an UNCONDITIONED Deny whose
//     Resource matches that ARN (a conditioned Deny can be stepped around by
//     the same principal that controls the path);
//   - none of the Deny statements that refuse impostors matches a genuine ARN,
//     and every genuine ARN is still reached by a cudly- prefix-scoped Allow.
func assertPathBearingARNsDenied(t *testing.T, files []string, impostors, genuine []pathGuardCase) {
	t.Helper()

	type stmtRef struct {
		file, body string
	}
	var allows, denies []stmtRef
	for _, f := range files {
		for _, s := range findStatements(t, f, func(string) bool { return true }) {
			switch statementEffect(s) {
			case "Allow":
				allows = append(allows, stmtRef{f, s})
			case "Deny":
				denies = append(denies, stmtRef{f, s})
			}
		}
	}

	prefixGrants := func(arn string) (actions []string, sids []string) {
		for _, a := range allows {
			matched := false
			for _, r := range statementResources(a.body) {
				if strings.Contains(r, "cudly-") && iamARNMatches(r, arn) {
					matched = true
				}
			}
			if !matched {
				continue
			}
			sids = append(sids, a.file+":"+statementSid(a.body))
			for _, act := range statementActions(a.body) {
				if isIAMAction(act) {
					actions = append(actions, act)
				}
			}
		}
		return actions, sids
	}

	pathDenies := map[string]stmtRef{} // keyed by file:Sid, for reporting
	for _, imp := range impostors {
		actions, sids := prefixGrants(imp.arn)
		if imp.requireGrant && len(actions) == 0 {
			t.Fatalf("no cudly- prefix-scoped Allow in %v matches the impostor %q; either the prefix scopes moved (update this guard) or the matcher is broken, and either way the checks below would pass vacuously", files, imp.arn)
		}
		for _, act := range actions {
			covered := false
			for _, d := range denies {
				if statementConditionBody(d.body) != "" {
					continue
				}
				resourceHit := false
				for _, r := range statementResources(d.body) {
					if iamARNMatches(r, imp.arn) {
						resourceHit = true
					}
				}
				if resourceHit && actionPermitted(statementActions(d.body), act) {
					covered = true
					pathDenies[d.file+":"+statementSid(d.body)] = d
				}
			}
			if !covered {
				t.Errorf("%s is granted on the path-bearing impostor %q by %v and no unconditioned Deny refuses it. In an IAM ARN `*` matches `/`, so a cudly-* scope is satisfied by a role, policy or instance profile created at path /cudly-x/ (#197); add the action to, or restore, the cudly-*/* Deny", act, imp.arn, sids)
			}
		}
	}

	for _, g := range genuine {
		for key, d := range pathDenies {
			for _, r := range statementResources(d.body) {
				if iamARNMatches(r, g.arn) {
					t.Errorf("%s: Deny Resource %q matches the genuine root-path ARN %q. The path guard must refuse ONLY path-bearing ARNs (cudly-*/*); matching a real role breaks the deploy while passing the impostor check", key, r, g.arn)
				}
			}
		}
		if g.requireGrant {
			if actions, _ := prefixGrants(g.arn); len(actions) == 0 {
				t.Errorf("no cudly- prefix-scoped Allow in %v matches the genuine ARN %q any more; the real deploy resources must keep matching their grant, or the fix passes its test by refusing everything", files, g.arn)
			}
		}
	}
}

// Representative ARNs. The genuine ones follow the naming in terraform/modules
// (stack_name = cudly-<env>-<hex>, name or name_prefix appended, root path).
var (
	impostorRoleARN            = "arn:aws:iam::123456789012:role/cudly-x/EvilRole"
	impostorDeepRoleARN        = "arn:aws:iam::123456789012:role/cudly-a/b/c/EvilRole"
	impostorPolicyARN          = "arn:aws:iam::123456789012:policy/cudly-x/EvilPolicy"
	impostorInstanceProfileARN = "arn:aws:iam::123456789012:instance-profile/cudly-x/EvilProfile"

	genuineLambdaRoleARN      = "arn:aws:iam::123456789012:role/cudly-prod-1a2b3c4d-lambda-20260101000000000000000001"
	genuineTaskExecRoleARN    = "arn:aws:iam::123456789012:role/cudly-dev-1a2b3c4d-task-execution"
	genuineSecretReadPolicy   = "arn:aws:iam::123456789012:policy/cudly-dev-1a2b3c4d-secret-read-20260101000000000000000001"
	genuineFckNatInstanceProf = "arn:aws:iam::123456789012:instance-profile/cudly-dev-1a2b3c4d-fck-nat-20260101000000000000000001"
)

// TestDeployPolicyDeniesPathBearingCudlyARNs pins IAMDenyPathBearingCudlyResources
// in policy_iam.tf against every cudly- prefix-scoped grant to the deploy role.
func TestDeployPolicyDeniesPathBearingCudlyARNs(t *testing.T) {
	assertPathBearingARNsDenied(t, deployPolicyFiles(t),
		[]pathGuardCase{
			{impostorRoleARN, true},
			{impostorDeepRoleARN, true},
			{impostorPolicyARN, true},
			{impostorInstanceProfileARN, true},
		},
		[]pathGuardCase{
			{genuineLambdaRoleARN, true},
			{genuineTaskExecRoleARN, true},
			{genuineSecretReadPolicy, true},
			{genuineFckNatInstanceProf, true},
		},
	)
}

// TestBoundaryDeniesPassingPathBearingRoles pins DenyPassPathBearingRole in
// policy_boundary.tf against PassRoleCeiling: a boundaried workload role must
// not be able to pass an out-of-band, unboundaried cudly-x/... role.
func TestBoundaryDeniesPassingPathBearingRoles(t *testing.T) {
	assertPathBearingARNsDenied(t, []string{boundaryFile},
		[]pathGuardCase{
			{impostorRoleARN, true},
			{impostorDeepRoleARN, true},
		},
		[]pathGuardCase{
			{genuineLambdaRoleARN, true},
			{genuineTaskExecRoleARN, true},
		},
	)
}

// iamPathedResourcePattern matches the head of every IAM resource type whose
// ARN carries a path.
var iamPathedResourcePattern = regexp.MustCompile(`(?m)^resource\s+"(aws_iam_role|aws_iam_policy|aws_iam_instance_profile)"\s+"([A-Za-z0-9_-]+)"\s*\{`)

// pathArgumentPattern matches a `path = ...` argument and captures its value.
var pathArgumentPattern = regexp.MustCompile(`(?m)^[ \t]*path\s*=\s*(.+?)\s*$`)

// TestCudlyIAMResourcesUseRootPath asserts that no IAM role, policy or
// instance profile on the deploy path, or in this bootstrap root, sets a path
// other than "/". The cudly-*/* Denies above are exact only under that
// premise: a resource this repo creates at /cudly-x/ would be refused by them
// (an apply-time 403), and one at any other path would fall outside the
// cudly-* grants altogether. Either way a path is a deliberate design change
// that has to revisit #197, not a drive-by attribute.
func TestCudlyIAMResourcesUseRootPath(t *testing.T) {
	ownFiles, err := filepath.Glob("*.tf")
	if err != nil {
		t.Fatalf("globbing *.tf: %v", err)
	}
	files := append(findAwsModuleFiles(t), ownFiles...)

	counts := map[string]int{}
	for _, f := range files {
		content := readPolicySource(t, f)
		for _, loc := range iamPathedResourcePattern.FindAllStringSubmatchIndex(content, -1) {
			kind, name := content[loc[2]:loc[3]], content[loc[4]:loc[5]]
			counts[kind]++
			open := loc[1] - 1
			block := content[open:balancedBraceEnd(content, open)]
			for _, m := range pathArgumentPattern.FindAllStringSubmatch(block, -1) {
				if m[1] != `"/"` {
					t.Errorf("%s: resource %q %q sets path = %s. Every cudly-* IAM resource must sit at the root path: the cudly-*/* Denies (IAMDenyPathBearingCudlyResources in policy_iam.tf, DenyPassPathBearingRole in policy_boundary.tf) refuse path-bearing ARNs because `*` in a cudly-* scope also matches the path (#197)", f, kind, name, m[1])
				}
			}
		}
	}
	for _, kind := range []string{"aws_iam_role", "aws_iam_policy", "aws_iam_instance_profile"} {
		if counts[kind] == 0 {
			t.Errorf("found zero %s resources across %d files; the scan is broken and would pass vacuously", kind, len(files))
		}
	}
}
