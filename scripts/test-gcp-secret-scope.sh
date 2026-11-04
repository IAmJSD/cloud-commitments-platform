#!/usr/bin/env bash
# test-gcp-secret-scope.sh
#
# Exercises check-gcp-secret-scope.sh against testdata fixtures, in BOTH
# directions: a clean input must pass and a violating input must fail. A guard
# that only ever returns "clean" is indistinguishable from a broken one, so the
# failing cases are the ones that give this check its value.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHECK="${SCRIPT_DIR}/check-gcp-secret-scope.sh"
FIXTURES="${SCRIPT_DIR}/testdata/gcp-secret-scope"

pass=0
fail=0

run_case() {
  local label="$1"
  local expected_exit="$2"
  shift 2

  local actual_exit=0
  "$CHECK" "$@" >/dev/null 2>&1 || actual_exit=$?

  if [[ "$actual_exit" -eq "$expected_exit" ]]; then
    echo "PASS: $label"
    (( pass++ )) || true
  else
    echo "FAIL: $label (expected exit $expected_exit, got $actual_exit)"
    (( fail++ )) || true
  fi
}

# Like run_case, but also asserts WHICH lines are reported: exactly the
# `role =` lines of the file, in order. One fixture can then hold many
# variants without a variant the guard misses hiding behind another it catches.
run_role_lines_case() {
  local label="$1"
  local file="$2"
  local expected actual out actual_exit=0
  expected=$(grep -n '^[[:space:]]*role[[:space:]]*=' "$file" | cut -d: -f1)
  out=$("$CHECK" "$file" 2>&1 >/dev/null) || actual_exit=$?
  actual=$(awk -v p="${file}:" 'index($0, p) == 1 {
             s = substr($0, length(p) + 1); sub(/:.*/, "", s); print s
           }' <<<"$out")
  if [[ "$actual_exit" -eq 1 && -n "$expected" && "$actual" == "$expected" ]]; then
    echo "PASS: $label"
    (( pass++ )) || true
  else
    echo "FAIL: $label (exit $actual_exit; role lines: $(tr '\n' ' ' <<<"$expected")" \
         "; reported: $(tr '\n' ' ' <<<"$actual"))"
    (( fail++ )) || true
  fi
}

# Negative direction: the supported shapes must not be reported. This is what
# catches a guard so blunt it fires on every per-secret binding.
run_case "per-secret binding and non-secret project grant exit 0" 0 \
  "${FIXTURES}/clean.tf.fixture"

# Positive direction: the anti-pattern must be caught.
run_case "project-scope secretAccessor exits 1" 1 \
  "${FIXTURES}/project-scope.tf.fixture"

run_case "project-scope _binding variant exits 1" 1 \
  "${FIXTURES}/project-binding.tf.fixture"

# Folder and organization scope are asserted separately. They used to share one
# fixture, which meant a guard that caught only one of them still passed.
run_case "folder-scope _member variant exits 1" 1 \
  "${FIXTURES}/folder-scope.tf.fixture"

run_case "organization-scope _binding variant exits 1" 1 \
  "${FIXTURES}/org-scope.tf.fixture"

# A violation must still be found when mixed in with clean files.
run_case "violation alongside a clean file exits 1" 1 \
  "${FIXTURES}/clean.tf.fixture" "${FIXTURES}/project-scope.tf.fixture"

# Usage errors are exit 2, distinct from "found a violation" (exit 1), so CI can
# tell a broken invocation apart from a real finding.
run_case "missing file exits 2" 2 \
  "${FIXTURES}/does-not-exist.tf.fixture"

run_case "unknown flag exits 2" 2 --bogus-flag

# The JSON encoding is not parseable by this scanner, so it must be rejected
# rather than reported clean. Exit 2 (cannot check) rather than 0 (checked and
# clean) is the whole point.
run_case "tf.json is rejected rather than silently passed" 2 \
  "${FIXTURES}/json-encoding.tf.json"

# Issue #149: every way a role can reach a scope-wide grant without its literal
# appearing inside the IAM resource block. Each of these exited 0 on the guard
# before #149 (it only looked inside scope-wide resource blocks).
run_case "locals set + for_each + each.key exits 1 (#149)" 1 \
  "${FIXTURES}/locals-for-each.tf.fixture"

run_case "for_each over a map, each.value.role exits 1" 1 \
  "${FIXTURES}/for-each-map.tf.fixture"

run_case "variable default fed to for_each exits 1" 1 \
  "${FIXTURES}/variable-default.tf.fixture"

run_case "role passed as a module argument exits 1" 1 \
  "${FIXTURES}/module-argument.tf.fixture"

run_case "role supplied by a tfvars file exits 1" 1 \
  "${FIXTURES}/roles.tfvars.fixture"

# These three carried the authoritative-policy shape and exited 1 through the
# role text alone. Since issue #150 that shape is refused outright (exit 2).
run_case "dynamic binding in google_iam_policy over a local exits 2 (#150)" 2 \
  "${FIXTURES}/dynamic-iam-policy.tf.fixture"

run_case "literal role in google_iam_policy data block exits 2 (#150)" 2 \
  "${FIXTURES}/iam-policy-data.tf.fixture"

run_case "JSON heredoc policy_data on google_project_iam_policy exits 2 (#150)" 2 \
  "${FIXTURES}/policy-heredoc.tf.fixture"

run_case "custom role carrying secretmanager.* permissions exits 1" 1 \
  "${FIXTURES}/custom-role-permission.tf.fixture"

run_case "block not laid out the way terraform fmt does exits 1" 1 \
  "${FIXTURES}/unformatted-block.tf.fixture"

# Issue #150, part 1: the authoritative-policy shape is refused (exit 2) rather
# than analyzed. Each of the first six exited 0 before #150, because the roles
# it grants never appear as `secretmanager` text in a scanned file.
run_case "google_project_iam_policy fed by file() exits 2 (#150)" 2 \
  "${FIXTURES}/project-iam-policy-file.tf.fixture"

run_case "google_folder_iam_policy fed by a variable exits 2 (#150)" 2 \
  "${FIXTURES}/folder-iam-policy.tf.fixture"

run_case "google_organization_iam_policy fed by jsonencode exits 2 (#150)" 2 \
  "${FIXTURES}/org-iam-policy.tf.fixture"

run_case "google_iam_policy data source exported as an output exits 2 (#150)" 2 \
  "${FIXTURES}/iam-policy-data-output.tf.fixture"

run_case "authoritative policy with bare block labels exits 2 (#150)" 2 \
  "${FIXTURES}/iam-policy-unquoted-labels.tf.fixture"

run_case "authoritative policy with a column-0 body exits 2 (#150)" 2 \
  "${FIXTURES}/iam-policy-unindented.tf.fixture"

run_case "rejected shape alongside a clean file still exits 2 (#150)" 2 \
  "${FIXTURES}/clean.tf.fixture" "${FIXTURES}/project-iam-policy-file.tf.fixture"

# Negative direction: lookalike type names and comments are not refused.
run_case "lookalike IAM policy types and comments exit 0 (#150)" 0 \
  "${FIXTURES}/iam-policy-lookalikes.tf.fixture"

# Issue #150, part 2: a body laid out in column 0 must not end a block early
# enough to drop a role, nor carry the per-secret exemption onward.
run_case "scope-wide grant with a column-0 body exits 1 (#150)" 1 \
  "${FIXTURES}/unindented-body.tf.fixture"

run_case "column-0 per-secret body does not leak its exemption (#150)" 1 \
  "${FIXTURES}/per-secret-unindented-body.tf.fixture"

# Fail closed on role names the scanner cannot resolve.
run_case "role assembled by interpolation exits 1" 1 \
  "${FIXTURES}/interpolated-role.tf.fixture"

run_case "role assembled by format() exits 1" 1 \
  "${FIXTURES}/format-role.tf.fixture"

# The scanner's own failure modes, each pushed the closed way.
run_case "a # inside a string does not hide the role after it" 1 \
  "${FIXTURES}/hash-in-string.tf.fixture"

run_case "per-secret exemption does not leak past its block" 1 \
  "${FIXTURES}/per-secret-then-local.tf.fixture"

run_case "per-secret exemption ends at the block's closing brace" 1 \
  "${FIXTURES}/per-secret-then-bare-attribute.tf.fixture"

run_case "per-secret exemption covers only a plain role line" 1 \
  "${FIXTURES}/per-secret-non-role-line.tf.fixture"

run_case "a skewed brace depth does not carry the exemption onward" 1 \
  "${FIXTURES}/brace-skew.tf.fixture"

run_case "a column-0 closing brace ends the exemption despite a skewed depth" 1 \
  "${FIXTURES}/brace-skew-bare-attribute.tf.fixture"

# Waivers, in both directions. A waiver must excuse the line directly below it
# and nothing else, and one that excuses nothing is itself a finding.
run_case "waiver with a reason directly above the role exits 0" 0 \
  "${FIXTURES}/waiver-valid.tf.fixture"

run_case "waiver without a reason exits 1" 1 \
  "${FIXTURES}/waiver-no-reason.tf.fixture"

run_case "waiver above a line with nothing to excuse exits 1" 1 \
  "${FIXTURES}/waiver-vacuous.tf.fixture"

run_case "waiver covers only the next line, not a second role" 1 \
  "${FIXTURES}/waiver-one-line-only.tf.fixture"

# Issue #505: a per-secret binding is exempt only for the two roles a workload
# needs on one secret, secretAccessor and secretVersionAdder. Before #505 any
# roles/secretmanager.* name passed there, so the #504 review's mutation of a
# version-adder grant to roles/secretmanager.admin exited 0.
run_case "allowed per-secret roles in every per-secret resource type exit 0 (#505)" 0 \
  "${FIXTURES}/per-secret-allowed-roles.tf.fixture"

run_case "per-secret roles/secretmanager.admin exits 1 (#505)" 1 \
  "${FIXTURES}/per-secret-admin.tf.fixture"

run_role_lines_case "every disallowed or lookalike per-secret role is reported (#505)" \
  "${FIXTURES}/per-secret-role-lookalikes.tf.fixture"

run_case "waived per-secret role outside the allowed pair exits 0 (#505)" 0 \
  "${FIXTURES}/per-secret-role-waived.tf.fixture"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Issue #149 acceptance: the deploy service account's project-scope
# roles/secretmanager.admin (reached through local.deploy_roles + for_each) is
# the one intentional scope-wide grant. It passes WITH its waiver and fails
# WITHOUT it. Asserting both directions is what stops the waiver going vacuous:
# if the role were removed or the guard stopped seeing it, the second case
# would flip to 0.
DEPLOY_SA="${REPO_ROOT}/terraform/environments/gcp/ci-cd-permissions/service_account.tf"
run_case "ci-cd-permissions deploy SA grant passes with its waiver" 0 "$DEPLOY_SA"

grep -v 'gcp-secret-scope: allow' "$DEPLOY_SA" > "${WORK}/service_account.tf"
if cmp -s "$DEPLOY_SA" "${WORK}/service_account.tf"; then
  echo "FAIL: ci-cd-permissions deploy SA file carries no waiver to remove"
  (( fail++ )) || true
fi
run_case "ci-cd-permissions deploy SA grant fails without its waiver" 1 \
  "${WORK}/service_account.tf"

# Issue #505 acceptance, against the real module the #504 review mutated: the
# GKE workload SA's version-adder grant on the admin password secret passes as
# written and fails once its role is widened to roles/secretmanager.admin.
GKE="${REPO_ROOT}/terraform/modules/compute/gcp/gke/main.tf"
sed 's|"roles/secretmanager.secretVersionAdder"|"roles/secretmanager.admin"|' \
  "$GKE" > "${WORK}/gke-main.tf"
if cmp -s "$GKE" "${WORK}/gke-main.tf"; then
  echo "FAIL: gke module carries no secretVersionAdder grant to widen"
  (( fail++ )) || true
fi
run_case "gke module per-secret grants pass as written (#505)" 0 "$GKE"
run_case "gke module version-adder widened to secretmanager.admin exits 1 (#505)" 1 \
  "${WORK}/gke-main.tf"

# The waiver is what lets per-secret-role-waived pass; without it, it fails.
grep -v 'gcp-secret-scope: allow' "${FIXTURES}/per-secret-role-waived.tf.fixture" \
  > "${WORK}/per-secret-role-unwaived.tf"
run_case "per-secret role outside the allowed pair fails without its waiver (#505)" 1 \
  "${WORK}/per-secret-role-unwaived.tf"

# policy-heredoc now exits 2 on its resource type alone, which would hide a
# regression in how heredoc bodies are scanned. Retyped to a per-secret
# _iam_policy (not refused), the role inside the JSON heredoc must still be
# reported: only a plain `role = "roles/secretmanager.X"` line is exempt.
sed 's/google_project_iam_policy/google_secret_manager_secret_iam_policy/g' \
  "${FIXTURES}/policy-heredoc.tf.fixture" > "${WORK}/per-secret-heredoc.tf"
run_case "JSON heredoc role on a per-secret _iam_policy exits 1" 1 \
  "${WORK}/per-secret-heredoc.tf"

# Issue #150, part 2: the guard's block tracking was written against
# `terraform fmt` layout, so CI must run `terraform fmt -check -recursive` over
# every root the guard scans. Read the roots from the guard itself so adding one without a fmt gate
# fails here.
CI_YML="${REPO_ROOT}/.github/workflows/ci.yml"
# shellcheck disable=SC2016  # ${REPO_ROOT} is matched literally, not expanded.
roots=$(sed -n '/^SCAN_ROOTS=(/,/^)/s|^ *"\${REPO_ROOT}/\([^"]*\)"$|\1|p' "$CHECK")
if [[ -z "$roots" ]]; then
  echo "FAIL: could not read SCAN_ROOTS from $CHECK"
  (( fail++ )) || true
fi
for root in $roots; do
  if grep -Eq "^[[:space:]]*(run:[[:space:]]*)?terraform fmt -check -recursive ${root}/?[[:space:]]*$" "$CI_YML"; then
    echo "PASS: CI runs terraform fmt -check over scan root ${root}/"
    (( pass++ )) || true
  else
    echo "FAIL: CI has no 'terraform fmt -check -recursive ${root}/' step for scan root ${root}/"
    (( fail++ )) || true
  fi
done

# Default-root discovery: run a copy of the guard from a throwaway tree laid out
# like the repo, so the cases below exercise which files it COLLECTS, not only
# how it scans files it is handed.
make_tree() {
  local dir="$1"
  mkdir -p "${dir}/scripts" "${dir}/terraform" "${dir}/iac"
  cp "$CHECK" "${dir}/scripts/check-gcp-secret-scope.sh"
  cp "${FIXTURES}/clean.tf.fixture" "${dir}/iac/main.tf"
}

run_tree_case() {
  local label="$1"
  local expected_exit="$2"
  local dir="$3"
  local actual_exit=0
  bash "${dir}/scripts/check-gcp-secret-scope.sh" >/dev/null 2>&1 || actual_exit=$?
  if [[ "$actual_exit" -eq "$expected_exit" ]]; then
    echo "PASS: $label"
    (( pass++ )) || true
  else
    echo "FAIL: $label (expected exit $expected_exit, got $actual_exit)"
    (( fail++ )) || true
  fi
}

make_tree "${WORK}/clean"
run_tree_case "default roots: clean tree exits 0" 0 "${WORK}/clean"

make_tree "${WORK}/tfvars"
cp "${FIXTURES}/roles.tfvars.fixture" "${WORK}/tfvars/terraform/prod.tfvars"
run_tree_case "default roots: tfvars files are scanned" 1 "${WORK}/tfvars"

make_tree "${WORK}/tfvars-example"
cp "${FIXTURES}/roles.tfvars.fixture" "${WORK}/tfvars-example/terraform/dev.tfvars.example"
run_tree_case "default roots: tfvars.example files are scanned" 1 "${WORK}/tfvars-example"

make_tree "${WORK}/tfvars-json"
echo '{}' > "${WORK}/tfvars-json/terraform/prod.tfvars.json"
run_tree_case "default roots: tfvars.json is rejected rather than passed" 2 "${WORK}/tfvars-json"

make_tree "${WORK}/iac-root"
cp "${FIXTURES}/locals-for-each.tf.fixture" "${WORK}/iac-root/iac/roles.tf"
run_tree_case "default roots: iac/ is scanned" 1 "${WORK}/iac-root"

# The real default scan roots (terraform/ + iac/, .tf and tfvars) must hold no
# un-waived Secret Manager mention outside a per-secret IAM resource. Unlike the
# guard before #149, this covers roles reaching a grant through locals,
# for_each, dynamic blocks, data policies, variable defaults, module arguments
# and tfvars. What it still cannot see is listed under LIMITATIONS in the guard.
run_case "default scan roots hold no un-waived Secret Manager grant" 0

echo ""
echo "Results: ${pass} passed, ${fail} failed."
[[ "$fail" -eq 0 ]]
