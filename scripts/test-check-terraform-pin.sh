#!/usr/bin/env bash
# test-check-terraform-pin.sh
#
# Exercises check-terraform-pin.sh in BOTH directions (a tree that reads the
# Terraform version from .terraform-version passes, each way of pinning it
# somewhere else fails) and terraform-version.sh, the reader every workflow
# job runs before hashicorp/setup-terraform: a valid file reaches
# $GITHUB_OUTPUT, and a missing or malformed one fails the step instead of
# leaving terraform_version empty (which setup-terraform installs as `latest`).
#
# Fixtures are throwaway git repos under a temp dir; nothing touches the real
# tree. Exits 0 when all cases pass; exits 1 on any failure.

# Fixture contents are literal workflow text (`${{ }}`) that must reach the
# files unexpanded, so single quotes are intended throughout.
# shellcheck disable=SC2016

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/check-terraform-pin.sh"
READER="${SCRIPT_DIR}/terraform-version.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Keep the user's git config (hooks, excludes, templates) out of the fixtures.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

pass=0
fail=0

record() {
  local label="$1" expected="$2" actual="$3"
  if [[ "$actual" -eq "$expected" ]]; then
    echo "PASS: $label"
    (( pass++ )) || true
  else
    echo "FAIL: $label (expected exit $expected, got $actual)"
    (( fail++ )) || true
  fi
}

run_case() {
  local label="$1" expected="$2"
  shift 2
  local actual=0
  bash "$CHECK" "$@" >/dev/null 2>&1 || actual=$?
  record "$label" "$expected" "$actual"
}

GOOD_JOB='jobs:
  deploy:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: Read Terraform version
        id: terraform-version
        run: bash scripts/terraform-version.sh
      - name: Setup Terraform
        uses: hashicorp/setup-terraform@dfe3c3f87815947d99a8997f908cb6525fc44e9e # v4.0.1
        with:
          terraform_version: ${{ steps.terraform-version.outputs.version }}'

# new_repo [WORKFLOW]: a fixture with a valid .terraform-version and one
# workflow (GOOD_JOB unless given). Sets REPO; files are tracked.
n=0
new_repo() {
  n=$((n + 1))
  REPO="${WORK}/repo-${n}"
  mkdir -p "${REPO}/.github/workflows"
  git -C "$REPO" init -q >/dev/null 2>&1
  printf '1.10.5\n' > "${REPO}/.terraform-version"
  printf '%s\n' "${1:-$GOOD_JOB}" > "${REPO}/.github/workflows/deploy.yml"
  git -C "$REPO" add -A
}

# ---- clean trees ------------------------------------------------------------

new_repo
run_case "a workflow that reads .terraform-version exits 0" 0 "$REPO"

new_repo "${GOOD_JOB}
  validate:
    steps:
      - name: Read Terraform version
        id: terraform-version
        run: bash scripts/terraform-version.sh
      - uses: hashicorp/setup-terraform@dfe3c3f87815947d99a8997f908cb6525fc44e9e # v4.0.1
        with:
          terraform_version: \${{ steps.terraform-version.outputs.version }} # from the file
          terraform_wrapper: false"
run_case "two jobs that each read it (one with a trailing comment) exit 0" 0 "$REPO"

# ---- pins outside .terraform-version ------------------------------------------

new_repo "env:
  TF_VERSION: '1.10.0'
${GOOD_JOB}"
run_case "a TF_VERSION env pin exits 1" 1 "$REPO"

new_repo "${GOOD_JOB/'${{ steps.terraform-version.outputs.version }}'/1.10.0}"
run_case "a literal terraform_version (1.10.0) exits 1" 1 "$REPO"

new_repo "${GOOD_JOB/'${{ steps.terraform-version.outputs.version }}'/'\${{ env.TF_VERSION }}'}"
run_case "terraform_version from env.TF_VERSION exits 1" 1 "$REPO"

new_repo "${GOOD_JOB/'${{ steps.terraform-version.outputs.version }}'/latest}"
run_case "terraform_version: latest exits 1" 1 "$REPO"

new_repo "${GOOD_JOB/'
        with:
          terraform_version: ${{ steps.terraform-version.outputs.version }}'/}"
run_case "a setup-terraform step with no terraform_version (installs latest) exits 1" 1 "$REPO"

new_repo "${GOOD_JOB/'      - name: Read Terraform version
        id: terraform-version
        run: bash scripts/terraform-version.sh
'/}"
run_case "a setup-terraform step whose job never reads the file exits 1" 1 "$REPO"

new_repo "${GOOD_JOB}
  validate:
    steps:
      - uses: hashicorp/setup-terraform@dfe3c3f87815947d99a8997f908cb6525fc44e9e # v4.0.1
        with:
          terraform_version: 1.10.5"
run_case "a second job with its own literal pin exits 1" 1 "$REPO"

new_repo
printf '%s\n' "${GOOD_JOB/'${{ steps.terraform-version.outputs.version }}'/1.10.0}" \
  > "${REPO}/.github/workflows/rollback.yaml"
git -C "$REPO" add -A
run_case "a literal pin in a .yaml workflow exits 1" 1 "$REPO"

new_repo 'jobs:
  lint:
    steps:
      - run: echo no terraform here'
run_case "a tree with no setup-terraform use at all exits 1" 1 "$REPO"

# ---- the version file itself --------------------------------------------------

new_repo
git -C "$REPO" rm -q -f .terraform-version
run_case "a missing .terraform-version exits 1" 1 "$REPO"

new_repo
git -C "$REPO" rm -q --cached .terraform-version
run_case "an untracked .terraform-version exits 1" 1 "$REPO"

for bad in '' 'latest' 'v1.10.5' '1.10' '~> 1.10' '<1.11.0' '1.10.5 # pinned' $'1.10.5\n1.11.0' $'1.10.5\r'; do
  new_repo
  printf '%s\n' "$bad" > "${REPO}/.terraform-version"
  git -C "$REPO" add -A
  run_case "a malformed .terraform-version ($(printf '%q' "$bad")) exits 1" 1 "$REPO"
done

# ---- usage errors -------------------------------------------------------------

new_repo
mkdir -p "${REPO}/sub"
run_case "unknown flag exits 2" 2 --bogus
run_case "two arguments exit 2" 2 "$REPO" "$REPO"
run_case "a missing directory exits 2" 2 "${WORK}/does-not-exist"
run_case "a directory below the work tree top level exits 2" 2 "${REPO}/sub"

# ---- the reader -----------------------------------------------------------------

# reader_case LABEL CONTENT EXPECTED_EXIT EXPECTED_OUTPUT_FILE_CONTENT
reader_case() {
  local label="$1" content="$2" expected="$3" want="$4"
  local dir="${WORK}/reader-${n}" actual=0 got
  n=$((n + 1))
  mkdir -p "$dir"
  if [[ "$content" != "<missing>" ]]; then
    printf '%s\n' "$content" > "${dir}/.terraform-version"
  fi
  : > "${dir}/github-output"
  GITHUB_OUTPUT="${dir}/github-output" bash "$READER" "$dir" >/dev/null 2>&1 || actual=$?
  got="$(cat "${dir}/github-output")"
  if [[ "$actual" -eq "$expected" && "$got" == "$want" ]]; then
    record "$label" "$expected" "$actual"
  else
    echo "FAIL: $label (exit $actual, want $expected; GITHUB_OUTPUT '$got', want '$want')"
    (( fail++ )) || true
  fi
}
reader_case "the reader writes version=<file> to GITHUB_OUTPUT" '1.10.5' 0 'version=1.10.5'
reader_case "the reader fails, writing nothing, when the file is missing" '<missing>' 1 ''
reader_case "the reader fails, writing nothing, on an empty file" '' 1 ''
reader_case "the reader fails, writing nothing, on a constraint (~> 1.10)" '~> 1.10' 1 ''

# ---- the real tree --------------------------------------------------------------

run_case "this repository reads Terraform only from .terraform-version" 0

echo ""
echo "Results: ${pass} passed, ${fail} failed."
[[ "$fail" -eq 0 ]]
