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

# seed_fixture <dest> copies the six compared files from the repo into a
# fixture tree that mirrors the repo layout.
seed_fixture() {
  local dest="$1"
  local files=(
    cloudformation/stacks/CUDly/template.yaml
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
# actions while the CFN stack grants them.
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

# Case 11: a required read missing from all three runtime copies (parity holds) must fail.
FIX11="$TMP_BASE/drift-required-runtime"
seed_fixture "$FIX11"
for f in cloudformation/stacks/CUDly/template.yaml terraform/modules/compute/aws/lambda/main.tf terraform/modules/compute/aws/fargate/main.tf; do
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

echo ""
echo "Results: ${pass} passed, ${fail} failed."
[[ "$fail" -eq 0 ]]
