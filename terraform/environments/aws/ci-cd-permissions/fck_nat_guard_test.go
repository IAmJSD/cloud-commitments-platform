package cicdpermissions_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// networkInterfaceActions are the EC2 actions the module's
// aws_network_interface.fck_nat (#716) needs on the deploy role. The
// ModifyNetworkInterfaceAttribute call is easy to miss: the provider turns
// source_dest_check off in a second call after the create. Without any one of
// these, plan stays green and the first apply that creates, updates or
// replaces the ENI fails with AccessDenied.
var networkInterfaceActions = []string{
	"ec2:CreateNetworkInterface",
	"ec2:DeleteNetworkInterface",
	"ec2:DescribeNetworkInterfaces",
	"ec2:ModifyNetworkInterfaceAttribute",
	"ec2:DetachNetworkInterface",
	"ec2:CreateTags",
	"ec2:DeleteTags",
}

func TestDeployPolicyAllowsFckNatNetworkInterface(t *testing.T) {
	var allowed []string
	var createTagsStmts []string
	for _, file := range deployPolicyFiles(t) {
		for _, stmt := range findStatements(t, file, func(s string) bool { return statementEffect(s) == "Allow" }) {
			actions := statementActions(stmt)
			allowed = append(allowed, actions...)
			if actionPermitted(actions, "ec2:CreateTags") {
				createTagsStmts = append(createTagsStmts, stmt)
			}
		}
	}
	for _, action := range networkInterfaceActions {
		if !actionPermitted(allowed, action) {
			t.Errorf("no deploy policy document allows %s, which aws_network_interface.fck_nat needs (#716)", action)
		}
	}

	// A CreateTags grant conditioned on ec2:CreateAction must include
	// CreateNetworkInterface, or tagging the ENI at creation is denied.
	// At least one grant must also be usable for it.
	usable := false
	for _, stmt := range createTagsStmts {
		cond := statementConditionBody(stmt)
		if cond == "" || (strings.Contains(cond, "ec2:CreateAction") && strings.Contains(cond, "CreateNetworkInterface")) {
			usable = true
			continue
		}
		if strings.Contains(cond, "ec2:CreateAction") {
			t.Errorf("statement %q grants ec2:CreateTags conditioned on ec2:CreateAction without CreateNetworkInterface; tagging the fck-nat ENI at create time would be denied", statementSid(stmt))
		}
	}
	if !usable {
		t.Errorf("no ec2:CreateTags grant is unconditioned or allows ec2:CreateAction=CreateNetworkInterface")
	}
}

// resourceBlock returns the body of `resource "type" "name" {...}` from
// comment-stripped HCL, or `resource "type" {...}` when name is empty.
func resourceBlock(t *testing.T, src, typ, name string) string {
	t.Helper()
	head := `resource "` + typ + `"`
	if name != "" {
		head += ` "` + name + `"`
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(head) + ` \{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("block %s not found in networking main.tf", head)
	}
	open := loc[1] - 1
	return src[open:balancedBraceEnd(src, open)]
}

// bracketValue returns the text of the first `key = [ ... ]` list in block,
// matching nested brackets (index expressions like fck_nat[0]).
func bracketValue(t *testing.T, block, key string) string {
	t.Helper()
	loc := regexp.MustCompile(regexp.QuoteMeta(key) + `\s*=\s*\[`).FindStringIndex(block)
	if loc == nil {
		return ""
	}
	depth := 0
	for i := loc[1] - 1; i < len(block); i++ {
		switch block[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return block[loc[1]:i]
			}
		}
	}
	return ""
}

// TestFckNatApplyOrderAndWiring pins HCL that mock-provider terraform tests
// cannot see (they expose no apply ordering) and that fails silently in a
// live account: a wrong order only shows as an egress outage.
func TestFckNatApplyOrderAndWiring(t *testing.T) {
	path := filepath.Join(modulesDir, "networking", "aws", "main.tf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := stripCommentLines(string(raw))

	route := resourceBlock(t, src, "aws_route", "private_internet_ipv4")
	asg := resourceBlock(t, src, "aws_autoscaling_group", "fck_nat")
	eni := resourceBlock(t, src, "aws_network_interface", "fck_nat")
	policy := resourceBlock(t, src, "aws_iam_role_policy", "fck_nat_ec2")

	dependsOn := func(block, target string) bool {
		for _, dep := range strings.Split(bracketValue(t, block, "depends_on"), ",") {
			if strings.TrimSpace(dep) == target {
				return true
			}
		}
		return false
	}
	if !dependsOn(route, "aws_autoscaling_group.fck_nat") {
		t.Error("aws_route.private_internet_ipv4 must depends_on aws_autoscaling_group.fck_nat: otherwise the routes move to the new ENI before an instance that attaches it is InService")
	}
	if !dependsOn(asg, "aws_iam_role_policy.fck_nat_ec2") {
		t.Error("aws_autoscaling_group.fck_nat must depends_on aws_iam_role_policy.fck_nat_ec2: the attach permission must exist before an instance boots")
	}

	if !regexp.MustCompile(`vpc_zone_identifier\s*=\s*\[aws_network_interface\.fck_nat\[0\]\.subnet_id\]`).MatchString(asg) {
		t.Error("the fck-nat ASG must launch only into the persistent ENI's subnet; an ENI attaches only within its own AZ")
	}
	if !regexp.MustCompile(`subnet_id\s*=\s*local\.public_subnet_ids\[0\]`).MatchString(eni) {
		t.Error("the persistent ENI must live in local.public_subnet_ids[0]")
	}

	if got := strings.TrimSpace(bracketValue(t, asg, "replace_triggered_by")); got != "aws_launch_template.fck_nat[count.index].user_data" {
		t.Errorf("replace_triggered_by must be exactly [aws_launch_template.fck_nat[count.index].user_data] (not the ENI, which would also fire on unrelated ENI attribute changes, and not image_id); got [%s]", got)
	}

	sid := strings.Index(policy, `Sid      = "AttachPersistentENI"`)
	if sid < 0 {
		t.Fatal("AttachPersistentENI statement not found in aws_iam_role_policy.fck_nat_ec2")
	}
	stmt := policy[sid:]
	if end := strings.Index(stmt, "},"); end >= 0 {
		stmt = stmt[:end]
	}
	res := bracketValue(t, stmt, "Resource")
	if res == "" {
		t.Fatalf("AttachPersistentENI Resource must be a list (a bare \"*\" string is rejected); statement: %s", stmt)
	}
	if !strings.Contains(res, "aws_network_interface.fck_nat[0].arn") {
		t.Errorf("AttachNetworkInterface must be scoped to the persistent ENI ARN; got %s", res)
	}
	if !strings.Contains(res, ":instance/*") {
		t.Errorf("AttachNetworkInterface is authorized against the instance too; Resource must include an instance/* ARN; got %s", res)
	}
	if regexp.MustCompile(`"\*"`).MatchString(res) {
		t.Errorf("AttachNetworkInterface Resource must not be \"*\"; got %s", res)
	}
}
