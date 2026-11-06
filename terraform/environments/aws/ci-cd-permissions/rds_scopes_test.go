package cicdpermissions_test

import (
	"strings"
	"testing"
)

// realRDSARNs are real ARN shapes per RDS resource type, as the Service
// Authorization Reference defines them (Amazon RDS, resource types table,
// https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazonrds.html,
// machine-readable form https://servicereference.us-east-1.amazonaws.com/v1/rds/rds.json,
// rds.json read 2026-11-04). Proxy and target-group ids are server-assigned and
// opaque (prx-..., prx-tg-...), never the resource name, and the proxy type's
// ARN segment is db-proxy, not proxy (#157).
var realRDSARNs = map[string]string{
	"db":           "arn:aws:rds:us-east-1:123456789012:db:cudly-prod-abc-postgres",
	"subgrp":       "arn:aws:rds:us-east-1:123456789012:subgrp:cudly-prod-abc-db-subnet",
	"snapshot":     "arn:aws:rds:us-east-1:123456789012:snapshot:cudly-prod-abc-snap",
	"db-proxy":     "arn:aws:rds:us-east-1:123456789012:db-proxy:prx-0123456789abcdef",
	"target-group": "arn:aws:rds:us-east-1:123456789012:target-group:prx-tg-0123456789abcdef",
}

// requiredRDSTypes lists, for each RDS action the deploy policy keeps, the
// resource types the SAR marks required (*) for it: IAM authorizes the call
// against each of them, so a grant that leaves one unmatched is dead. Hand
// copied from the SAR RDS actions table (URL above) because rds.json does not
// mark required types and optional types (pg, og, secgrp on CreateDBInstance)
// work without a grant. The required markers come from rds.json plus knowledge
// of the HTML page, which could not be rendered to verify them; recheck against
// the page when adding an action. The tagging actions list many optional types and mark
// none required, so they only need some matching entry.
var requiredRDSTypes = map[string][]string{
	"rds:CreateDBInstance":       {"db"},
	"rds:CreateDBSnapshot":       {"db", "snapshot"},
	"rds:CreateDBSubnetGroup":    {"subgrp"},
	"rds:DeleteDBInstance":       {"db"},
	"rds:DeleteDBSubnetGroup":    {"subgrp"},
	"rds:ModifyDBInstance":       {"db"},
	"rds:ModifyDBSubnetGroup":    {"subgrp"},
	"rds:AddTagsToResource":      nil,
	"rds:ListTagsForResource":    nil,
	"rds:RemoveTagsFromResource": nil,
}

func matchesAnyRealRDSARN(pattern string) bool {
	for _, arn := range realRDSARNs {
		if iamARNMatches(pattern, arn) {
			return true
		}
	}
	return false
}

// rdsStatements returns the Allow statements of policy_data.tf that grant any
// rds: action.
func rdsStatements(t *testing.T) []string {
	t.Helper()
	return findStatements(t, "policy_data.tf", func(stmt string) bool {
		if statementEffect(stmt) != "Allow" {
			return false
		}
		for _, a := range statementActions(stmt) {
			if strings.HasPrefix(a, "rds:") {
				return true
			}
		}
		return false
	})
}

func TestRDSResourceScopedPatternsMatchRealARNs(t *testing.T) {
	stmts := rdsStatements(t)
	if len(stmts) == 0 {
		t.Fatal("found no rds: Allow statements in policy_data.tf; the test would pass vacuously")
	}
	for _, stmt := range stmts {
		for _, r := range statementResources(stmt) {
			if r != "*" && !matchesAnyRealRDSARN(r) {
				t.Errorf("statement %q: Resource %q matches no real RDS ARN, so every action scoped only by it is a dead grant (#157)", statementSid(stmt), r)
			}
		}
	}
}

func TestRDSKeptActionsHaveEveryRequiredResourceType(t *testing.T) {
	allow := rdsStatements(t)
	// A newly granted action must get a table entry, or it goes unchecked.
	for _, stmt := range allow {
		for _, a := range statementActions(stmt) {
			_, kept := requiredRDSTypes[a]
			_, proxy := requiredRDSProxyTypes[a]
			if !kept && !proxy && !strings.HasPrefix(a, "rds:Describe") && a != "rds:CreateDBProxy" {
				t.Errorf("statement %q grants %s, which has no entry in requiredRDSTypes; add its required resource types", statementSid(stmt), a)
			}
		}
	}
	for action, types := range requiredRDSTypes {
		if len(types) == 0 {
			if !grantedBy(allow, grantNeed{action: action, arn: realRDSARNs["db"]}) {
				t.Errorf("%s: no statement grants it on a real db ARN", action)
			}
			continue
		}
		for _, typ := range types {
			if !grantedBy(allow, grantNeed{action: action, arn: realRDSARNs[typ]}) {
				t.Errorf("%s: required resource type %s (%s) is not granted", action, typ, realRDSARNs[typ])
			}
		}
	}
}

// requiredRDSProxyTypes lists the resource types the SAR (Amazon RDS actions
// table, https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazonrds.html,
// machine-readable https://servicereference.us-east-1.amazonaws.com/v1/rds/rds.json,
// read 2026-11-05) requires for each proxy action. rds.json names the proxy
// type "proxy" (ARN segment db-proxy) and does not mark required types;
// DeregisterDBProxyTargets lists cluster, db, proxy and target-group, of which
// the proxy and target group are the ones a proxy-scoped grant must cover.
// These ids are opaque (prx-..., prx-tg-...), so a cudly-* name scope cannot
// express them. Not granted until enable_rds_proxy is turned on (see
// rds_proxy_tripwire_test.go).
var requiredRDSProxyTypes = map[string][]string{
	"rds:DeleteDBProxy":            {"db-proxy"},
	"rds:ModifyDBProxy":            {"db-proxy"},
	"rds:RegisterDBProxyTargets":   {"db", "target-group"},
	"rds:DeregisterDBProxyTargets": {"db", "db-proxy", "target-group"},
	"rds:ModifyDBProxyTargetGroup": {"target-group"},
}

// TestRDSProxyMutatingActionsNeedProxyARNs: a proxy action, once granted, must
// be authorizable against every resource type it requires, and no statement
// granting it may match none of them (a dead grant).
func TestRDSProxyMutatingActionsNeedProxyARNs(t *testing.T) {
	allow := deployAllowStatements(t)
	for action, types := range requiredRDSProxyTypes {
		var holders []string
		for _, stmt := range allow {
			if actionPermitted(statementActions(stmt), action) {
				holders = append(holders, stmt)
			}
		}
		if len(holders) == 0 {
			continue // not granted yet; the tripwire covers that
		}
		for _, stmt := range holders {
			matched := false
			for _, typ := range types {
				for _, r := range statementResources(stmt) {
					matched = matched || iamARNMatches(r, realRDSARNs[typ])
				}
			}
			if !matched {
				t.Errorf("statement %q grants %s but its Resource matches none of %v; it is a dead grant (#157)", statementSid(stmt), action, types)
			}
		}
		for _, typ := range types {
			if !grantedBy(holders, grantNeed{action: action, arn: realRDSARNs[typ]}) {
				t.Errorf("%s: required resource type %s (%s) is not granted", action, typ, realRDSARNs[typ])
			}
		}
	}
}
