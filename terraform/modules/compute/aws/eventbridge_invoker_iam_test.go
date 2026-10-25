// Guard for issue #292: every identity-policy grant of ecs:RunTask must be
// pinned to one cluster, and every iam:PassRole granted alongside it must be
// pinned to ECS tasks.
//
// The fargate module defines four EventBridge invoker roles with the same
// purpose and four hand-maintained copies of the same policy. Two of them
// carried both conditions and two carried neither, so those two could run the
// task definition in any cluster in the account and pass the task role, which
// holds the SP/RI purchase grants, to any service that accepts a passed role.
// The copies diverged once already; this guard DISCOVERS every RunTask grant
// by walking the tree (the same walk as the #1636 guard) rather than naming the
// four policies, so a fifth copy added later is held to the same rule.
//
// fargate/tests/eventbridge_invoker_iam.tftest.hcl asserts the same thing on
// the RENDERED policies of the four known roles; this file is what reaches the
// copies nobody has written a run block for yet.
package aws_test

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// knownInvokerPolicies are the aws_iam_role_policy resources the walk must
// find in fargate/main.tf. Not the guard's input (the walk is) but its floor:
// if the action is respelled in a form runTaskActionPattern misses, the walk
// finds nothing and the guard reports a clean tree by looking at nothing.
var knownInvokerPolicies = []string{
	"eventbridge",
	"eventbridge_fire_scheduled_purchases",
	"eventbridge_ladder_run",
	"eventbridge_ri_exchange",
}

var fargateMainTF = filepath.Join("terraform", "modules", "compute", "aws", "fargate", "main.tf")

// runTaskActionPattern matches an ecs:RunTask grant as a quoted HCL action.
// IAM action names are case-insensitive, and ecs:* and ecs:Run* grant RunTask
// as surely as the literal does.
var runTaskActionPattern = regexp.MustCompile(`(?i)"ecs:(?:runtask|run\*|\*)"`)

var passRoleActionPattern = regexp.MustCompile(`(?i)"iam:(?:passrole|pass\*|\*)"`)

var (
	allowEffect = regexp.MustCompile(`\bEffect\s*=\s*"Allow"`)

	// The condition must name the module's own cluster by reference. A literal
	// pattern such as "arn:aws:ecs:*:*:cluster/*" would satisfy a presence
	// check while constraining nothing.
	clusterCondition = regexp.MustCompile(`\bArnEquals\s*=\s*\{\s*"ecs:cluster"\s*=\s*aws_ecs_cluster\.\w+\.arn\s*\}`)

	passedToECSTasks = regexp.MustCompile(`\bStringEquals\s*=\s*\{\s*"iam:PassedToService"\s*=\s*"ecs-tasks\.amazonaws\.com"\s*\}`)

	// RunTask must name a task definition and PassRole must name roles, both
	// by reference. Any quoted Resource, "*" above all, fails these.
	taskDefinitionResource = regexp.MustCompile(`\bResource\s*=\s*aws_ecs_task_definition\.\w+\.arn\s*(?:\n|$)`)
	roleListResource       = regexp.MustCompile(`\bResource\s*=\s*\[\s*aws_iam_role\.\w+\.arn(?:\s*,\s*aws_iam_role\.\w+\.arn)*\s*,?\s*\]`)

	policyResourceHeader = regexp.MustCompile(`resource\s+"aws_iam_role_policy"\s+"(\w+)"`)
)

// runTaskGrant is one Allow statement granting ecs:RunTask, plus the PassRole
// statements of the same Statement list.
type runTaskGrant struct {
	path      string
	policy    string
	statement string
	passRoles []string
}

func (g runTaskGrant) String() string { return g.path + ": " + g.policy }

// enclosing returns the bounds of the innermost open..close pair around idx.
// Each bracket type is balanced on its own in the HCL under test (Action and
// Resource lists use [], statement objects and conditions use {}), so the
// other type can be ignored.
func enclosing(content string, idx int, open, closeCh byte) (int, int) {
	start, depth := -1, 0
	for i := idx - 1; i >= 0; i-- {
		if content[i] == closeCh {
			depth++
			continue
		}
		if content[i] != open {
			continue
		}
		if depth == 0 {
			start = i
			break
		}
		depth--
	}
	if start == -1 {
		return -1, -1
	}
	depth = 0
	for i := start; i < len(content); i++ {
		switch content[i] {
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return start, i + 1
			}
		}
	}
	return -1, -1
}

// policyName is the aws_iam_role_policy resource most recently opened before
// idx, or "" when the grant is written some other way.
func policyName(content string, idx int) string {
	all := policyResourceHeader.FindAllStringSubmatch(content[:idx], -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// statementAt returns the statement object holding the action at idx. ok is
// false when no balanced object encloses it, which the caller reports rather
// than skips: an unparsed grant must not read as a conditioned one.
func statementAt(content string, idx int) (string, int, bool) {
	start, end := enclosing(content, idx, '{', '}')
	if start == -1 {
		return "", -1, false
	}
	return content[start:end], start, true
}

// passRolesBeside returns the PassRole statements in the Statement list that
// holds the statement starting at stmtStart.
func passRolesBeside(content string, stmtStart int) []string {
	listStart, listEnd := enclosing(content, stmtStart, '[', ']')
	if listStart == -1 {
		return nil
	}
	var out []string
	for _, loc := range passRoleActionPattern.FindAllStringIndex(content[listStart:listEnd], -1) {
		if stmt, _, ok := statementAt(content, listStart+loc[0]); ok {
			out = append(out, stmt)
		}
	}
	return out
}

// runTaskGrants walks grantRoots and returns every Allow statement granting
// RunTask. The permissions boundary (skippedGrantFiles) holds RunTask as a
// ceiling, not a grant, and is excluded by the walk.
func runTaskGrants(t *testing.T) []runTaskGrant {
	t.Helper()

	root := repoRoot(t)
	var grants []runTaskGrant
	for _, rel := range grantRoots {
		walkPolicyFiles(t, root, filepath.Join(root, rel), func(path, content string) {
			code := stripCommentLines(content)
			for _, loc := range runTaskActionPattern.FindAllStringIndex(code, -1) {
				stmt, start, ok := statementAt(code, loc[0])
				if !ok {
					t.Errorf("%s: found an ecs:RunTask grant at offset %d that is not inside a statement object this guard can read; rewrite it in the jsonencode form the fargate module uses, or teach the guard the new form", path, loc[0])
					continue
				}
				if !allowEffect.MatchString(stmt) {
					continue
				}
				grants = append(grants, runTaskGrant{
					path:      path,
					policy:    policyName(code, loc[0]),
					statement: stmt,
					passRoles: passRolesBeside(code, start),
				})
			}
		})
	}
	return grants
}

func TestRunTaskGrantsArePinnedToClusterAndECSTasks(t *testing.T) {
	grants := runTaskGrants(t)

	found := map[string]bool{}
	for _, g := range grants {
		if g.path == fargateMainTF {
			found[g.policy] = true
		}
		assertRunTaskGrantScoped(t, g)
	}

	var missing []string
	for _, name := range knownInvokerPolicies {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%s: the walk did not find ecs:RunTask in aws_iam_role_policy %s; either the policy moved or the action is spelled in a form runTaskActionPattern misses, and the guard would report clean while inspecting nothing", fargateMainTF, strings.Join(missing, ", "))
	}
}

func assertRunTaskGrantScoped(t *testing.T, g runTaskGrant) {
	t.Helper()

	if !clusterCondition.MatchString(g.statement) {
		t.Errorf("%s grants ecs:RunTask without Condition ArnEquals \"ecs:cluster\" = aws_ecs_cluster.<name>.arn. Without it the role can run the task definition in ANY cluster in the account (#292)", g)
	}
	if !taskDefinitionResource.MatchString(g.statement) {
		t.Errorf("%s grants ecs:RunTask on a Resource other than a single aws_ecs_task_definition.<name>.arn reference (#292)", g)
	}
	if len(g.passRoles) == 0 {
		t.Errorf("%s grants ecs:RunTask with no iam:PassRole statement beside it; RunTask needs PassRole on the task and execution roles even without overrides, so this policy cannot run the task, or this guard did not find the PassRole it was meant to check", g)
	}
	for _, stmt := range g.passRoles {
		if !passedToECSTasks.MatchString(stmt) {
			t.Errorf("%s grants iam:PassRole beside ecs:RunTask without Condition StringEquals \"iam:PassedToService\" = \"ecs-tasks.amazonaws.com\". Without it the role can hand the task role (and its SP/RI purchase grants) to any service that accepts a passed role (#292)", g)
		}
		if !roleListResource.MatchString(stmt) {
			t.Errorf("%s grants iam:PassRole beside ecs:RunTask on a Resource other than a list of aws_iam_role.<name>.arn references (#292)", g)
		}
	}
}

// TestRunTaskGuardRejectsUnconditionedGrants feeds the guard's matchers the
// pre-fix shape of the recommendations policy and the fixed shape, so a
// matcher loosened until it matches everything fails here even while the tree
// is clean.
func TestRunTaskGuardRejectsUnconditionedGrants(t *testing.T) {
	const unconditioned = `
resource "aws_iam_role_policy" "eventbridge" {
  policy = jsonencode({
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ecs:RunTask"
        ]
        Resource = aws_ecs_task_definition.main.arn
      },
      {
        Effect = "Allow"
        Action = [
          "iam:PassRole"
        ]
        Resource = [
          aws_iam_role.task_execution.arn,
          aws_iam_role.task.arn
        ]
      }
    ]
  })
}
`
	const conditioned = `
resource "aws_iam_role_policy" "eventbridge" {
  policy = jsonencode({
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ecs:RunTask"
        ]
        Resource = aws_ecs_task_definition.main.arn
        Condition = {
          ArnEquals = {
            "ecs:cluster" = aws_ecs_cluster.main.arn
          }
        }
      },
      {
        Effect = "Allow"
        Action = [
          "iam:PassRole"
        ]
        Resource = [
          aws_iam_role.task_execution.arn,
          aws_iam_role.task.arn
        ]
        Condition = {
          StringEquals = {
            "iam:PassedToService" = "ecs-tasks.amazonaws.com"
          }
        }
      }
    ]
  })
}
`
	for _, tc := range []struct {
		name           string
		code           string
		wantConditions bool
	}{
		{"pre-fix recommendations policy", unconditioned, false},
		{"fixed recommendations policy", conditioned, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locs := runTaskActionPattern.FindAllStringIndex(tc.code, -1)
			if len(locs) != 1 {
				t.Fatalf("found %d RunTask actions, want 1", len(locs))
			}
			stmt, start, ok := statementAt(tc.code, locs[0][0])
			if !ok {
				t.Fatal("statementAt did not find the RunTask statement")
			}
			if got := policyName(tc.code, locs[0][0]); got != "eventbridge" {
				t.Errorf("policyName = %q, want %q", got, "eventbridge")
			}
			passRoles := passRolesBeside(tc.code, start)
			if len(passRoles) != 1 {
				t.Fatalf("found %d PassRole statements beside RunTask, want 1", len(passRoles))
			}
			// Both shapes scope Resource correctly; only the conditions differ,
			// and each matcher must tell them apart on its own.
			if got := clusterCondition.MatchString(stmt); got != tc.wantConditions {
				t.Errorf("clusterCondition matched=%v, want %v", got, tc.wantConditions)
			}
			if got := passedToECSTasks.MatchString(passRoles[0]); got != tc.wantConditions {
				t.Errorf("passedToECSTasks matched=%v, want %v", got, tc.wantConditions)
			}
			if !taskDefinitionResource.MatchString(stmt) {
				t.Error("taskDefinitionResource did not match aws_ecs_task_definition.main.arn")
			}
			if !roleListResource.MatchString(passRoles[0]) {
				t.Error("roleListResource did not match the task and task-execution role ARNs")
			}
		})
	}
}
