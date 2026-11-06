package cicdpermissions_test

import "testing"

// grantNeed is one action a Terraform apply performs, with a sample real ARN
// of the resource it runs against. noResource marks an action the SAR gives no
// resource type (action-only); without it an empty arn is a bug in the need and
// fails instead of matching any statement.
type grantNeed struct {
	action     string
	arn        string
	noResource bool
}

// grantedBy reports whether any statement in allow authorizes need.
func grantedBy(allow []string, need grantNeed) bool {
	for _, stmt := range allow {
		if !actionPermitted(statementActions(stmt), need.action) {
			continue
		}
		if need.noResource {
			return true
		}
		if need.arn == "" {
			return false
		}
		for _, r := range statementResources(stmt) {
			if iamARNMatches(r, need.arn) {
				return true
			}
		}
	}
	return false
}

// deployAllowStatements returns every Allow statement of the deploy-role
// documents. The boundary caps the workload roles and its broad grants
// (logs:*, rds:*) would mask a missing deploy grant, so it is excluded.
func deployAllowStatements(t *testing.T) []string {
	t.Helper()
	var allow []string
	for _, f := range deployPolicyFiles(t) {
		for _, stmt := range extractStatementBlocks(readPolicySource(t, f)) {
			if statementEffect(stmt) == "Allow" {
				allow = append(allow, stmt)
			}
		}
	}
	if len(allow) == 0 {
		t.Fatal("found no Allow statements; the statement splitter is broken and this test would pass vacuously")
	}
	return allow
}

// assertGranted fails for every need that no deploy Allow statement authorizes.
// Deny statements are ignored: none touches rds, route53, acm, logs or sns
// today, so a future Deny on those services must be a conscious change here.
func assertGranted(t *testing.T, label string, needs []grantNeed) {
	t.Helper()
	allow := deployAllowStatements(t)
	for _, n := range needs {
		if !grantedBy(allow, n) {
			t.Errorf("%s: no deploy policy statement grants %s on %q", label, n.action, n.arn)
		}
	}
}

func TestGrantedBy(t *testing.T) {
	stmt := func(resource string) string {
		return `{
        Effect   = "Allow"
        Action   = ["rds:ModifyDBProxy"]
        Resource = ` + resource + `
      }`
	}
	const proxy = "arn:aws:rds:us-east-1:123456789012:db-proxy:prx-0123456789abcdef"
	cases := []struct {
		name string
		stmt string
		need grantNeed
		want bool
	}{
		{"action only", stmt(`"*"`), grantNeed{action: "rds:ModifyDBProxy", noResource: true}, true},
		{"wrong action", stmt(`"*"`), grantNeed{action: "rds:DeleteDBProxy", noResource: true}, false},
		{"real arn", stmt(`["arn:aws:rds:*:*:db-proxy:*"]`), grantNeed{action: "rds:ModifyDBProxy", arn: proxy}, true},
		{"dead arn scope", stmt(`["arn:aws:rds:*:*:proxy:cudly-*"]`), grantNeed{action: "rds:ModifyDBProxy", arn: proxy}, false},
		{"empty arn is not action-only", stmt(`"*"`), grantNeed{action: "rds:ModifyDBProxy"}, false},
	}
	for _, c := range cases {
		if got := grantedBy([]string{c.stmt}, c.need); got != c.want {
			t.Errorf("%s: grantedBy = %v, want %v", c.name, got, c.want)
		}
	}
}
