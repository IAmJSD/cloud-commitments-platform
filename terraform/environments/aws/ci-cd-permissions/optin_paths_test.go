package cicdpermissions_test

import "testing"

// TestDeployPolicyGrantsTagAndRetentionRemoval covers calls the AWS provider
// makes when an attribute is REMOVED rather than set, which a first apply never
// exercises (#157). Real ARN shapes per the Service Authorization Reference.
func TestDeployPolicyGrantsTagAndRetentionRemoval(t *testing.T) {
	const logs = "arn:aws:logs:us-east-1:123456789012:log-group:"
	assertGranted(t, "acm/logs removal", []grantNeed{
		// aws_acm_certificate.frontend drops a tag key (environments/aws/acm.tf).
		{action: "acm:RemoveTagsFromCertificate", arn: "arn:aws:acm:us-east-1:123456789012:certificate/11111111-2222-3333-4444-555555555555"},
		// A log group retention going from N days to 0 (never expire).
		{action: "logs:DeleteRetentionPolicy", arn: logs + "/aws/lambda/cudly-prod-abc-api-20260101"},
		{action: "logs:DeleteRetentionPolicy", arn: logs + "/ecs/cudly-prod-abc"},
		{action: "logs:DeleteRetentionPolicy", arn: logs + "/aws/vpc/cudly-prod-abc"},
	})
}

// TestPutRetentionPolicyIsPairedWithDelete: a statement that can set a log
// retention must also be able to clear it, or lowering it to 0 fails the apply.
func TestPutRetentionPolicyIsPairedWithDelete(t *testing.T) {
	seen := false
	for _, stmt := range deployAllowStatements(t) {
		if !actionPermitted(statementActions(stmt), "logs:PutRetentionPolicy") {
			continue
		}
		seen = true
		if !actionPermitted(statementActions(stmt), "logs:DeleteRetentionPolicy") {
			t.Errorf("statement %q grants logs:PutRetentionPolicy without logs:DeleteRetentionPolicy", statementSid(stmt))
		}
	}
	if !seen {
		t.Fatal("no statement grants logs:PutRetentionPolicy; the pairing check would pass vacuously")
	}
}

// TestDeployPolicyGrantsHostedZoneLifecycle covers create_subdomain_zone = true
// (environments/aws/route53.tf). CreateHostedZone has no resource type; the
// hostedzone ARN carries an opaque id and the SAR defines no condition keys for
// it, so neither a name scope nor a tag gate is possible (#157).
func TestDeployPolicyGrantsHostedZoneLifecycle(t *testing.T) {
	const zone = "arn:aws:route53:::hostedzone/Z0123456789ABCDEFGHIJ"
	assertGranted(t, "route53 zone", []grantNeed{
		{action: "route53:CreateHostedZone", noResource: true},
		{action: "route53:ChangeTagsForResource", arn: zone},
		{action: "route53:DeleteHostedZone", arn: zone},
	})
}

// resourcesOf returns the Resource entries of every deploy Allow statement that
// grants action, failing when none does (a vacuous bound).
func resourcesOf(t *testing.T, action string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, stmt := range deployAllowStatements(t) {
		if actionPermitted(statementActions(stmt), action) {
			out[statementSid(stmt)] = statementResources(stmt)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no deploy statement grants %s; its upper-bound check would pass vacuously", action)
	}
	return out
}

// TestNewActionsAreNotGrantedOnWildcard is the upper bound for the #157
// additions: they stay off Resource "*" where the SAR gives them a resource
// type. route53:CreateHostedZone has none, so it is exempt. The ACM actions
// stay on "*" like their siblings DeleteCertificate and AddTagsToCertificate,
// which this PR must not reshape: acm:RemoveTagsFromCertificate is exempt here
// and a tag gate for the whole ACM group is a follow-up.
func TestNewActionsAreNotGrantedOnWildcard(t *testing.T) {
	for _, action := range []string{
		"logs:DeleteRetentionPolicy",
		"route53:DeleteHostedZone",
		"route53:ChangeTagsForResource",
	} {
		for sid, resources := range resourcesOf(t, action) {
			for _, r := range resources {
				if r == "*" {
					t.Errorf("statement %q grants %s on Resource \"*\"; the SAR gives it a resource type", sid, action)
				}
			}
		}
	}
}

func TestRetentionRemovalStaysOnCudlyLogGroups(t *testing.T) {
	prefixes := []string{
		"arn:aws:logs:*:*:log-group:/aws/lambda/cudly-",
		"arn:aws:logs:*:*:log-group:/aws/vpc/cudly-",
		"arn:aws:logs:*:*:log-group:/ecs/cudly-",
	}
	for sid, resources := range resourcesOf(t, "logs:DeleteRetentionPolicy") {
		for _, r := range resources {
			ok := false
			for _, p := range prefixes {
				ok = ok || (len(r) > len(p) && r[:len(p)] == p)
			}
			if !ok {
				t.Errorf("statement %q grants logs:DeleteRetentionPolicy on %q, outside the cudly log-group prefixes", sid, r)
			}
		}
	}
}

func TestHostedZoneActionsStayOnHostedZoneARNs(t *testing.T) {
	const want = "arn:aws:route53:::hostedzone/*"
	for _, action := range []string{"route53:DeleteHostedZone", "route53:ChangeTagsForResource"} {
		for sid, resources := range resourcesOf(t, action) {
			for _, r := range resources {
				if r != want {
					t.Errorf("statement %q grants %s on %q, want only %q", sid, action, r, want)
				}
			}
		}
	}
}
