#!/usr/bin/env bash
# test-aws-iam-parity.sh
#
# Exercises check-aws-iam-parity.sh:
#   1. against the real repository (must pass post-reconciliation),
#   2. against fixture trees seeded from the real files with drift injected,
#      replicating the exact pre-fix INF-02 failure shapes (missing CE
#      Coverage actions in the Terraform Lambda module; missing org-discovery
#      statement in the federation CloudFormation template).
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHECK="${SCRIPT_DIR}/check-aws-iam-parity.sh"

pass=0
fail=0

run_case() {
  local label="$1"
  local expected_exit="$2"
  shift 2

  actual_exit=0
  bash "$CHECK" "$@" >/dev/null 2>&1 || actual_exit=$?

  if [[ "$actual_exit" -eq "$expected_exit" ]]; then
    echo "PASS: $label"
    (( pass++ )) || true
  else
    echo "FAIL: $label (expected exit $expected_exit, got $actual_exit)"
    (( fail++ )) || true
  fi
}

# seed_fixture <dest> copies the compared files from the repo into a
# fixture tree that mirrors the repo layout.
seed_fixture() {
  local dest="$1"
  local files=(
    terraform/modules/compute/aws/lambda/main.tf
    terraform/modules/compute/aws/fargate/main.tf
    iac/federation/aws-cross-account/cloudformation/template.yaml
    iac/federation/aws-cross-account/terraform/main.tf
    iac/federation/aws-target/cloudformation/template.yaml
    iac/federation/aws-target/terraform/main.tf
    internal/iacfiles/templates/aws-cross-account-cli.sh.tmpl
    internal/iacfiles/templates/aws-wif-cli.sh.tmpl
    cloudformation/stacks/CUDly-CrossAccount/template.yaml
  )
  local f
  for f in "${files[@]}"; do
    mkdir -p "$dest/$(dirname "$f")"
    cp "$REPO_ROOT/$f" "$dest/$f"
  done
}

TMP_BASE="$(mktemp -d)"
trap 'rm -rf "$TMP_BASE"' EXIT

# Case 1: the real repository must be in parity.
run_case "real repository is in parity" 0

# Case 2: pre-fix INF-02 shape - TF Lambda module missing the CE Coverage
# actions while the TF Fargate module grants them.
FIX2="$TMP_BASE/drift-runtime"
seed_fixture "$FIX2"
grep -v 'ce:GetReservationCoverage\|ce:GetSavingsPlansCoverage' \
  "$REPO_ROOT/terraform/modules/compute/aws/lambda/main.tf" \
  > "$FIX2/terraform/modules/compute/aws/lambda/main.tf"
run_case "runtime drift (TF lambda missing CE Coverage actions) exits 1" 1 --root "$FIX2"

# Case 3: pre-fix INF-02 shape - federation CFN template missing the
# org-discovery statement its Terraform sibling carries.
FIX3="$TMP_BASE/drift-federation"
seed_fixture "$FIX3"
grep -v 'organizations:ListAccounts\|organizations:DescribeOrganization' \
  "$REPO_ROOT/iac/federation/aws-cross-account/cloudformation/template.yaml" \
  > "$FIX3/iac/federation/aws-cross-account/cloudformation/template.yaml"
run_case "federation drift (CFN missing org-discovery statement) exits 1" 1 --root "$FIX3"

# Case 4: a missing source file must fail loud (exit 2), never pass silently.
FIX4="$TMP_BASE/missing-file"
seed_fixture "$FIX4"
rm "$FIX4/terraform/modules/compute/aws/fargate/main.tf"
run_case "missing compared file exits 2" 2 --root "$FIX4"

# Case 5: --root without a path value must fail loud (exit 2), not crash on an
# unbound variable under set -u.
run_case "--root without a value exits 2" 2 --root

# Case 6 (#702): a federation file that loses the post-purchase tagging grant
# must fail, otherwise the purchase idempotency tag silently stops being
# written in member accounts.
FIX6="$TMP_BASE/drift-createtags"
seed_fixture "$FIX6"
grep -v 'ec2:CreateTags' \
  "$REPO_ROOT/iac/federation/aws-target/terraform/main.tf" \
  > "$FIX6/iac/federation/aws-target/terraform/main.tf"
run_case "federation drift (target TF missing ec2:CreateTags) exits 1" 1 --root "$FIX6"

# Case 7 (#702): same for a CloudFormation federation template.
FIX7="$TMP_BASE/drift-createtags-cfn"
seed_fixture "$FIX7"
grep -v 'ec2:CreateTags' \
  "$REPO_ROOT/iac/federation/aws-target/cloudformation/template.yaml" \
  > "$FIX7/iac/federation/aws-target/cloudformation/template.yaml"
run_case "federation drift (target CFN missing ec2:CreateTags) exits 1" 1 --root "$FIX7"

# Case 8 (#289): a required customer-account read missing from one policy only
# fails the required-actions check (the stack template is outside the parity groups).
FIX8="$TMP_BASE/drift-required-stack"
seed_fixture "$FIX8"
grep -v 'ec2:DescribeInstanceTypes$' \
  "$REPO_ROOT/cloudformation/stacks/CUDly-CrossAccount/template.yaml" \
  > "$FIX8/cloudformation/stacks/CUDly-CrossAccount/template.yaml"
run_case "required action (CrossAccount stack missing ec2:DescribeInstanceTypes) exits 1" 1 --root "$FIX8"

# Case 9 (#289): the same read missing from every flavor stays in parity but must still fail.
FIX9="$TMP_BASE/drift-required-all"
seed_fixture "$FIX9"
for f in iac/federation/aws-cross-account/cloudformation/template.yaml iac/federation/aws-cross-account/terraform/main.tf \
  iac/federation/aws-target/cloudformation/template.yaml iac/federation/aws-target/terraform/main.tf \
  internal/iacfiles/templates/aws-cross-account-cli.sh.tmpl internal/iacfiles/templates/aws-wif-cli.sh.tmpl; do
  sed 's/ce:GetCostAndUsage//' "$REPO_ROOT/$f" > "$FIX9/$f"
done
run_case "required action missing from all flavors (parity holds) exits 1" 1 --root "$FIX9"

# Case 10: a commented-out action is not a grant.
FIX10="$TMP_BASE/commented-out"
seed_fixture "$FIX10"
sed 's/^\( *\)- ec2:DescribeInstanceTypes$/\1# - ec2:DescribeInstanceTypes/' \
  "$REPO_ROOT/cloudformation/stacks/CUDly-CrossAccount/template.yaml" \
  > "$FIX10/cloudformation/stacks/CUDly-CrossAccount/template.yaml"
run_case "commented-out required action exits 1" 1 --root "$FIX10"

# Case 11: a required read missing from both runtime copies (parity holds) must fail.
FIX11="$TMP_BASE/drift-required-runtime"
seed_fixture "$FIX11"
for f in terraform/modules/compute/aws/lambda/main.tf terraform/modules/compute/aws/fargate/main.tf; do
  sed 's/ce:GetCostAndUsage//' "$REPO_ROOT/$f" > "$FIX11/$f"
done
run_case "required action missing from all runtime copies exits 1" 1 --root "$FIX11"

# Case 12 (#916): the Savings Plans offerings and RI exchange-quote reads missing from the CrossAccount stack.
FIX12="$TMP_BASE/drift-required-stack-sp-exchange"
seed_fixture "$FIX12"
grep -v 'savingsplans:DescribeSavingsPlansOfferings$\|ec2:GetReservedInstancesExchangeQuote$' \
  "$REPO_ROOT/cloudformation/stacks/CUDly-CrossAccount/template.yaml" \
  > "$FIX12/cloudformation/stacks/CUDly-CrossAccount/template.yaml"
run_case "CrossAccount stack missing SP offerings and exchange-quote reads exits 1" 1 --root "$FIX12"

# Case 13 (#302): widening one side of a pair to a service wildcard while the
# other keeps its explicit list must fail parity.
FIX13="$TMP_BASE/wildcard-service"
seed_fixture "$FIX13"
cp "$REPO_ROOT/terraform/modules/compute/aws/lambda/main.tf" "$FIX13/terraform/modules/compute/aws/lambda/main.tf"
printf '%s\n' '      "ec2:*",' >> "$FIX13/terraform/modules/compute/aws/lambda/main.tf"
run_case "wildcard drift (TF lambda grants ec2:*) exits 1" 1 --root "$FIX13"

# Case 14 (#302): same for a bare "*" Action.
FIX14="$TMP_BASE/wildcard-bare"
seed_fixture "$FIX14"
cp "$REPO_ROOT/terraform/modules/compute/aws/lambda/main.tf" "$FIX14/terraform/modules/compute/aws/lambda/main.tf"
printf '%s\n' '    "Action": "*",' >> "$FIX14/terraform/modules/compute/aws/lambda/main.tf"
run_case "wildcard drift (TF lambda grants Action \"*\") exits 1" 1 --root "$FIX14"

# Case 15 (#302): a Resource "*" is not an action wildcard; the real repo stays in parity.
FIX15="$TMP_BASE/resource-star"
seed_fixture "$FIX15"
cp "$REPO_ROOT/terraform/modules/compute/aws/lambda/main.tf" "$FIX15/terraform/modules/compute/aws/lambda/main.tf"
printf '%s\n' '    "Resource": "*",' >> "$FIX15/terraform/modules/compute/aws/lambda/main.tf"
run_case "Resource \"*\" is not treated as an action wildcard" 0 --root "$FIX15"

# Case 16 (#69): the pre-fix shape - both runtime copies grant the unused
# organizations:DescribeAccount / DescribeOrganization. Parity holds, so only
# the runtime organizations allowlist catches it.
FIX16="$TMP_BASE/dead-runtime-org-grants"
seed_fixture "$FIX16"
for f in terraform/modules/compute/aws/lambda/main.tf terraform/modules/compute/aws/fargate/main.tf; do
  sed 's/^\( *\)"organizations:ListAccounts",$/&\
\1"organizations:DescribeAccount",\
\1"organizations:DescribeOrganization",/' \
    "$REPO_ROOT/$f" > "$FIX16/$f"
done
for f in terraform/modules/compute/aws/lambda/main.tf terraform/modules/compute/aws/fargate/main.tf; do
  grep -q 'organizations:DescribeOrganization' "$FIX16/$f" || { echo "FAIL: case 16 fixture did not inject into $f"; (( fail++ )) || true; }
done
run_case "runtime grants unused organizations actions in both copies exits 1" 1 --root "$FIX16"

# --- #1136: bare "*" Action written across lines, NotAction, lookalikes -------
# Each case widens (or pretends to widen) one compared copy only, so parity
# fails exactly when the extractor reports a bare "*" for it. The YAML cases
# use the federation cross-account CloudFormation template, the YAML file in
# a parity group now that the CloudFormation install stack is retired (#1432).

FED_XACC_CFN_REL=iac/federation/aws-cross-account/cloudformation/template.yaml
TF_LAMBDA_REL=terraform/modules/compute/aws/lambda/main.tf
TF_FARGATE_REL=terraform/modules/compute/aws/fargate/main.tf

# new_fixture <name> seeds a fixture tree under TMP_BASE and prints its path.
new_fixture() {
  seed_fixture "$TMP_BASE/$1"
  echo "$TMP_BASE/$1"
}

# cfn_statement <fixture> inserts stdin as a statement of the federation
# cross-account role policy, ahead of the CostExplorer statement. A missing anchor counts
# as a failure, so a moved anchor cannot turn a case into a silent no-op.
cfn_statement() {
  local file="$1/$FED_XACC_CFN_REL" snip="$1/statement.snip"
  cat > "$snip"
  if awk -v snip="$snip" '
      !done && index($0, "- Sid: CostExplorer") { while ((getline l < snip) > 0) print l; done = 1 }
      { print }
      END { exit !done }' "$file" > "$file.new"; then
    mv "$file.new" "$file"
  else
    echo "FAIL: CostExplorer anchor not found in $file"
    (( fail++ )) || true
  fi
  rm -f "$snip" "$file.new"
}

# Case 17: YAML block list item.
FIX17=$(new_fixture bare-star-yaml-block)
cfn_statement "$FIX17" <<'YAML'
          - Sid: Widened
            Effect: Allow
            Action:
              - "*"
            Resource: "*"
YAML
run_case "bare \"*\" as a YAML block list item (federation CFN) exits 1" 1 --root "$FIX17"

# Case 18: YAML compact block list (items at the key's indent), single-quoted.
FIX18=$(new_fixture bare-star-yaml-compact)
cfn_statement "$FIX18" <<'YAML'
          - Sid: Widened
            Effect: Allow
            Action:
            - logs:CreateLogGroup
            - '*'
            Resource: "*"
YAML
run_case "bare '*' in a compact YAML block list (federation CFN) exits 1" 1 --root "$FIX18"

# Case 19: YAML flow list where "*" is not the first item.
FIX19=$(new_fixture bare-star-yaml-flow)
cfn_statement "$FIX19" <<'YAML'
          - Sid: Widened
            Effect: Allow
            Action: [logs:CreateLogGroup, "*"]
            Resource: "*"
YAML
run_case "bare \"*\" after another item in a YAML flow list (federation CFN) exits 1" 1 --root "$FIX19"

# Case 20: HCL policy-document actions list across lines.
FIX20=$(new_fixture bare-star-hcl-actions)
cat >> "$FIX20/$TF_LAMBDA_REL" <<'HCL'

data "aws_iam_policy_document" "widened" {
  statement {
    actions = [
      "*",
    ]
    resources = ["*"]
  }
}
HCL
run_case "bare \"*\" in a multi-line HCL actions list (TF lambda) exits 1" 1 --root "$FIX20"

# Case 21: jsonencode Action list across lines.
FIX21=$(new_fixture bare-star-hcl-jsonencode)
cat >> "$FIX21/$TF_FARGATE_REL" <<'HCL'

resource "aws_iam_role_policy" "widened" {
  role = "widened"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "logs:CreateLogGroup",
        "*",
      ]
      Resource = "*"
    }]
  })
}
HCL
run_case "bare \"*\" in a multi-line jsonencode Action list (TF fargate) exits 1" 1 --root "$FIX21"

# Case 22: JSON policy heredoc Action array across lines.
FIX22=$(new_fixture bare-star-json)
cat >> "$FIX22/$TF_FARGATE_REL" <<'HCL'

resource "aws_iam_policy" "widened" {
  policy = <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "*"
    ],
    "Resource": "*"
  }]
}
EOF
}
HCL
run_case "bare \"*\" in a multi-line JSON Action array (TF fargate) exits 1" 1 --root "$FIX22"

# Case 23: JSON with the array opening on the line after the key, in a CLI
# onboarding template (federation core group).
FIX23=$(new_fixture bare-star-json-cli)
sed 's/^\( *\)"Action": \["ec2:CreateTags"\]$/\1"Action":\
\1  [\
\1    "ec2:CreateTags",\
\1    "*"\
\1  ]/' "$REPO_ROOT/internal/iacfiles/templates/aws-wif-cli.sh.tmpl" \
  > "$FIX23/internal/iacfiles/templates/aws-wif-cli.sh.tmpl"
grep -qx ' *"\*"' "$FIX23/internal/iacfiles/templates/aws-wif-cli.sh.tmpl" \
  || { echo "FAIL: case 23 fixture did not inject into aws-wif-cli.sh.tmpl"; (( fail++ )) || true; }
run_case "bare \"*\" in a JSON array opened on the next line (WIF CLI template) exits 1" 1 --root "$FIX23"

# Case 24: NotAction "*" matches no action, so it grants nothing and is not
# an action wildcard (single-line and block forms). The first NotAction opens
# the statement right after an Action block list, so the block must end at
# the dedented "- " line rather than read it as one of its items.
FIX24=$(new_fixture notaction-yaml)
cfn_statement "$FIX24" <<'YAML'
          - Sid: AllowLogs
            Effect: Allow
            Resource: "*"
            Action:
              - logs:CreateLogGroup
          - NotAction: "*"
            Effect: Deny
            Resource: "*"
          - Sid: DenyNothing
            Effect: Deny
            NotAction: "*"
            Resource: "*"
          - Sid: DenyNothingBlock
            Effect: Deny
            NotAction:
              - "*"
            Resource: "*"
YAML
run_case "NotAction \"*\" (federation CFN) is not an action wildcard" 0 --root "$FIX24"

# Case 25: same for HCL not_actions and JSON/jsonencode NotAction.
FIX25=$(new_fixture notaction-hcl-json)
cat >> "$FIX25/$TF_LAMBDA_REL" <<'HCL'

data "aws_iam_policy_document" "deny_nothing" {
  statement {
    effect      = "Deny"
    not_actions = ["*"]
    resources   = ["*"]
  }
  statement {
    effect = "Deny"
    not_actions = [
      "*",
    ]
    resources = ["*"]
  }
}
HCL
cat >> "$FIX25/$TF_FARGATE_REL" <<'HCL'

resource "aws_iam_role_policy" "deny_nothing" {
  role = "deny-nothing"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Deny"
      NotAction = "*"
      Resource  = "*"
    }]
  })
}

resource "aws_iam_policy" "deny_nothing" {
  policy = <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Deny",
    "NotAction": [
      "*"
    ],
    "Resource": "*"
  }]
}
EOF
}
HCL
run_case "not_actions / NotAction \"*\" (TF lambda and fargate) is not an action wildcard" 0 --root "$FIX25"

# Case 26: YAML lookalikes. Service wildcards outside the compared namespaces
# and a "*" in the Resource or Condition lists that follow are not a bare "*".
FIX26=$(new_fixture lookalike-yaml)
cfn_statement "$FIX26" <<'YAML'
          - Sid: Lookalikes
            Effect: Allow
            Action:
              - "s3:*"
              - 'logs:Describe*'
            Resource:
              - "*"
            Condition:
              StringLike:
                aws:ResourceTag/Owner:
                  - "*"
YAML
run_case "YAML lookalikes (\"s3:*\", Resource/Condition \"*\" lists) are not a bare \"*\"" 0 --root "$FIX26"

# Case 27: HCL and JSON lookalikes, including a Resource list that opens on the
# line right after a closed Action list.
FIX27=$(new_fixture lookalike-hcl-json)
cat >> "$FIX27/$TF_LAMBDA_REL" <<'HCL'

resource "aws_iam_role_policy" "lookalikes" {
  role = "lookalikes"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "s3:*",
        "logs:Describe*",
      ]
      Resource = [
        "*",
      ]
      Condition = {
        StringLike = {
          "aws:ResourceTag/Owner" = [
            "*",
          ]
        }
      }
    }]
  })
}
HCL
cat >> "$FIX27/$TF_FARGATE_REL" <<'HCL'

resource "aws_iam_policy" "lookalikes" {
  policy = <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "s3:*"
    ],
    "Resource": [
      "*"
    ]
  }]
}
EOF
}
HCL
run_case "HCL/JSON lookalikes (\"s3:*\", Resource/Condition \"*\" lists) are not a bare \"*\"" 0 --root "$FIX27"

# Case 28: the line that ends an Action block list is itself scanned for keys,
# so a bare "*" Action opening the next statement is still caught.
FIX28=$(new_fixture bare-star-after-block)
cfn_statement "$FIX28" <<'YAML'
          - Sid: AllowLogs
            Effect: Allow
            Resource: "*"
            Action:
              - logs:CreateLogGroup
          - Action: "*"
            Effect: Allow
            Resource: "*"
YAML
run_case "bare \"*\" Action opening the statement after an Action block list (federation CFN) exits 1" 1 --root "$FIX28"

echo ""
echo "Results: ${pass} passed, ${fail} failed."
[[ "$fail" -eq 0 ]]
