package iacfiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const reservedInstanceARN = "arn:aws:ec2:*:*:reserved-instances/*"

type awsPolicyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

// permissionsPolicy extracts the PERMISSIONS_POLICY heredoc from an AWS CLI
// onboarding template (the heredoc is quoted and holds no template
// placeholders).
func permissionsPolicy(t *testing.T, path string) []awsPolicyStatement {
	t.Helper()
	raw, err := Templates.ReadFile(path)
	require.NoError(t, err)
	_, rest, found := strings.Cut(string(raw), "PERMISSIONS_POLICY=$(cat <<'JSON'\n")
	require.True(t, found, "PERMISSIONS_POLICY heredoc not found in %s", path)
	body, _, found := strings.Cut(rest, "\nJSON\n")
	require.True(t, found, "PERMISSIONS_POLICY heredoc terminator not found in %s", path)
	var doc struct {
		Statement []awsPolicyStatement `json:"Statement"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	return doc.Statement
}

// cfnStatements returns every mapping that has both an Action list and a
// Resource scalar, which is how a CloudFormation IAM statement looks.
func cfnStatements(t *testing.T, raw []byte) []awsPolicyStatement {
	t.Helper()
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal(raw, &root))
	var out []awsPolicyStatement
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			var stmt awsPolicyStatement
			var hasAction, hasResource bool
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i].Value, n.Content[i+1]
				switch {
				case key == "Action" && val.Kind == yaml.SequenceNode:
					hasAction = true
					for _, item := range val.Content {
						stmt.Action = append(stmt.Action, item.Value)
					}
				case key == "Resource" && val.Kind == yaml.ScalarNode:
					hasResource = true
					stmt.Resource = val.Value
				}
			}
			if hasAction && hasResource {
				out = append(out, stmt)
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out
}

// memberAccountPolicies lists the seven member-account purchase policies:
// the two CLI onboarding templates, the four federation templates and the
// legacy CUDly-CrossAccount stack.
func memberAccountPolicies(t *testing.T) map[string][]awsPolicyStatement {
	t.Helper()
	root := filepath.Join("..", "..")
	read := func(rel string) []byte {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err, rel)
		return raw
	}
	return map[string][]awsPolicyStatement{
		"templates/aws-cross-account-cli.sh.tmpl": permissionsPolicy(t, "templates/aws-cross-account-cli.sh.tmpl"),
		"templates/aws-wif-cli.sh.tmpl":           permissionsPolicy(t, "templates/aws-wif-cli.sh.tmpl"),

		"iac/federation/aws-cross-account/cloudformation/template.yaml": cfnStatements(t, read("iac/federation/aws-cross-account/cloudformation/template.yaml")),
		"iac/federation/aws-target/cloudformation/template.yaml":        cfnStatements(t, read("iac/federation/aws-target/cloudformation/template.yaml")),
		"cloudformation/stacks/CUDly-CrossAccount/template.yaml":        cfnStatements(t, read("cloudformation/stacks/CUDly-CrossAccount/template.yaml")),

		"iac/federation/aws-cross-account/terraform/main.tf": tfStatements(t, read("iac/federation/aws-cross-account/terraform/main.tf")),
		"iac/federation/aws-target/terraform/main.tf":        tfStatements(t, read("iac/federation/aws-target/terraform/main.tf")),
	}
}

// TestMemberAccountPoliciesTagReservedInstancesOnly pins #702 for every
// member-account policy: the role can tag the reserved instance it just bought
// (the purchase idempotency tag), but only on reserved-instances resources,
// never on every EC2 resource. redshift:CreateTags is deliberately absent: it
// cannot be scoped below "*" and whether Redshift can tag reserved nodes at all
// is unverified, so that half of #702 is still open.
func TestMemberAccountPoliciesTagReservedInstancesOnly(t *testing.T) {
	for path, statements := range memberAccountPolicies(t) {
		t.Run(path, func(t *testing.T) {
			require.NotEmpty(t, statements, "no policy statements parsed")
			assert.Empty(t, createTagsViolations(statements))
		})
	}
}

// createTagsViolations lists every way statements break the #702 tagging
// rule: ec2:CreateTags granted anywhere but reserved instances, granted other
// than exactly once, or redshift:CreateTags granted at all.
func createTagsViolations(statements []awsPolicyStatement) []string {
	var problems []string
	var createTags int
	for _, stmt := range statements {
		for _, action := range stmt.Action {
			switch action {
			case "ec2:CreateTags":
				createTags++
				if stmt.Resource != reservedInstanceARN {
					problems = append(problems, "ec2:CreateTags must be scoped to "+reservedInstanceARN+", got Resource "+strconv.Quote(stmt.Resource))
				}
			case "redshift:CreateTags":
				problems = append(problems, "redshift:CreateTags must not be granted until it is verified and condition-scoped (#702)")
			}
		}
	}
	if createTags != 1 {
		problems = append(problems, "ec2:CreateTags must appear exactly once, got "+strconv.Itoa(createTags))
	}
	return problems
}

// memberAccountTerraform lists the Terraform member-account policies, the
// only ones read through tfStatements.
var memberAccountTerraform = []string{
	"iac/federation/aws-cross-account/terraform/main.tf",
	"iac/federation/aws-target/terraform/main.tf",
}

// tfStatementMutants are edits to a member-account Terraform policy that widen
// ec2:CreateTags to every EC2 resource. Each is inserted as a new statement in
// front of the CostExplorer statement. None has a Sid, which is what the old
// Sid-anchored scan could not see (#782); the last is a Sid-carrying control.
var tfStatementMutants = map[string]string{
	"Sid-less statement": `{
          Effect   = "Allow"
          Action   = ["ec2:CreateTags"]
          Resource = "*"
        },
`,
	"Sid-less statement with a scalar Action": `{
          Effect   = "Allow"
          Action   = "ec2:CreateTags"
          Resource = "*"
        },
`,
	"Sid-less one-line statement": `{ Effect = "Allow", Action = ["ec2:CreateTags"], Resource = "*" },
`,
	"Sid-less statement with quoted keys and colons": `{
          "Effect": "Allow",
          "Action": ["ec2:CreateTags"],
          "Resource": ["*"],
        },
`,
	"Sid-less statement behind a comment holding braces": `# } closing brace in a comment {
        {
          Effect   = "Allow" # trailing comment }
          Action   = ["ec2:CreateTags"]
          Resource = "*"
        },
`,
	"control: statement with a Sid": `{
          Sid      = "Widened"
          Effect   = "Allow"
          Action   = ["ec2:CreateTags"]
          Resource = "*"
        },
`,
}

// withStatementBeforeCostExplorer returns src with stmt inserted as a new
// statement just before the one whose Sid is CostExplorer.
func withStatementBeforeCostExplorer(t *testing.T, src, stmt string) string {
	t.Helper()
	sid := strings.Index(src, `Sid    = "CostExplorer"`)
	require.Positive(t, sid, "CostExplorer statement not found")
	open := strings.LastIndex(src[:sid], "{")
	require.Positive(t, open, "CostExplorer statement has no opening brace")
	return src[:open] + stmt + "        " + src[open:]
}

// TestTFStatementsCatchWidenedCreateTags pins #782: a statement that grants
// ec2:CreateTags on every EC2 resource fails the #702 check whether or not it
// has a Sid. Every mutant is plain literal HCL, so the scan must read it (not
// refuse it) and the statements it returns must break the tagging rule.
func TestTFStatementsCatchWidenedCreateTags(t *testing.T) {
	for _, path := range memberAccountTerraform {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		require.NoError(t, err, path)
		for name, stmt := range tfStatementMutants {
			t.Run(path+"/"+name, func(t *testing.T) {
				mutant := withStatementBeforeCostExplorer(t, string(raw), stmt)
				statements, err := parseTFStatements([]byte(mutant))
				require.NoError(t, err, "every mutant is literal HCL the scan must read, not refuse")
				assert.NotEmpty(t, createTagsViolations(statements), "widened ec2:CreateTags went undetected")
			})
		}
		// aws_iam_policy_document statement blocks spell the keys actions and
		// resources and rarely carry a sid.
		t.Run(path+"/Sid-less aws_iam_policy_document statement block", func(t *testing.T) {
			mutant := string(raw) + `
data "aws_iam_policy_document" "widened" {
  statement {
    effect    = "Allow"
    actions   = ["ec2:CreateTags"]
    resources = ["*"]
  }
}
`
			statements, err := parseTFStatements([]byte(mutant))
			require.NoError(t, err)
			assert.NotEmpty(t, createTagsViolations(statements), "widened ec2:CreateTags went undetected")
		})
		// The real tagging statement loses its Sid and is widened in place, so
		// ec2:CreateTags still appears exactly once: only the Resource check can
		// catch it.
		t.Run(path+"/Sid-less tagging statement widened in place", func(t *testing.T) {
			mutant := regexp.MustCompile(`Sid\s*=\s*"EC2ReservedInstanceTagging"\n`).ReplaceAllString(string(raw), "")
			mutant = strings.Replace(mutant, `Resource = "`+reservedInstanceARN+`"`, `Resource = "*"`, 1)
			require.NotContains(t, mutant, reservedInstanceARN, "mutant did not widen the tagging statement")
			statements, err := parseTFStatements([]byte(mutant))
			require.NoError(t, err)
			assert.Contains(t, createTagsViolations(statements),
				`ec2:CreateTags must be scoped to `+reservedInstanceARN+`, got Resource "*"`)
		})
	}
}

// TestParseTFStatementsRefusesWhatItCannotRead pins the fail-closed half of
// #782: a statement the scan cannot read is an error, never a silent skip.
func TestParseTFStatementsRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]struct{ stmt, wantErr string }{
		"Action from a reference": {wantErr: "not a literal string", stmt: `{
          Effect   = "Allow"
          Action   = local.tag_actions
          Resource = "*"
        },
`},
		"interpolated Action": {wantErr: "not a literal string", stmt: `{
          Effect   = "Allow"
          Action   = ["${var.service}:CreateTags"]
          Resource = "*"
        },
`},
		"Resource from a reference": {wantErr: "not a literal string", stmt: `{
          Effect   = "Allow"
          Action   = ["ec2:CreateTags"]
          Resource = var.tag_resource
        },
`},
		"duplicate Action key": {wantErr: "repeats its Action or Resource key", stmt: `{
          Effect   = "Allow"
          Action   = ["ec2:CreateTags"]
          Action   = ["ec2:DescribeTags"]
          Resource = "*"
        },
`},
	}
	for _, path := range memberAccountTerraform {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		require.NoError(t, err, path)
		for name, tc := range cases {
			t.Run(path+"/"+name, func(t *testing.T) {
				_, err := parseTFStatements([]byte(withStatementBeforeCostExplorer(t, string(raw), tc.stmt)))
				assert.ErrorContains(t, err, tc.wantErr)
			})
		}
		t.Run(path+"/policy in a heredoc", func(t *testing.T) {
			heredoc := string(raw) + `
resource "aws_iam_policy" "widened" {
  policy = <<-EOT
    {"Statement": [{"Effect": "Allow", "Action": "ec2:CreateTags", "Resource": "*"}]}
  EOT
}
`
			_, err := parseTFStatements([]byte(heredoc))
			assert.ErrorContains(t, err, "Action keys")
		})
	}
}
