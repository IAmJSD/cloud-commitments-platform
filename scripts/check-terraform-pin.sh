#!/usr/bin/env bash
# check-terraform-pin.sh
#
# Fails when a workflow installs Terraform at a version that does not come
# from .terraform-version.
#
# Before issue #349 the version was written in ten workflows: ci.yml and
# pre-commit.yml validated on 1.10.5 while the six deploy, cleanup and destroy
# workflows applied on 1.10.0, so a change could pass CI and still break at
# apply time. Now .terraform-version is the only pin, and every job reads it
# through scripts/terraform-version.sh (which validates it) right before
# hashicorp/setup-terraform.
#
# Checked in every tracked workflow under .github/workflows/:
#
#   1. .terraform-version is present, tracked and MAJOR.MINOR.PATCH.
#   2. No TF_VERSION: the old per-workflow env pin.
#   3. Every `terraform_version:` input is exactly
#      ${{ steps.terraform-version.outputs.version }}.
#   4. Per file, the number of hashicorp/setup-terraform uses, of
#      terraform_version inputs that read the step output, and of
#      `bash scripts/terraform-version.sh` read steps are equal. A
#      setup-terraform step with no terraform_version installs `latest`, and
#      one without a read step in its job gets an empty version, which is
#      also `latest`.
#   5. At least one setup-terraform use exists, so a scan that matched
#      nothing cannot pass.
#
# Usage: check-terraform-pin.sh [ROOT]
#   ROOT defaults to this repository and must be the top level of a git work
#   tree. The self-test (scripts/test-check-terraform-pin.sh) points it at
#   fixtures.
#
# Exit codes: 0 clean, 1 findings, 2 usage or setup error.

set -euo pipefail

if [[ $# -gt 1 ]]; then
  echo "Usage: $0 [ROOT]" >&2
  exit 2
fi
case "${1:-}" in
  -*) echo "Unknown flag: $1" >&2; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
if [[ ! -d "$ROOT" ]]; then
  echo "ERROR: not a directory: $ROOT" >&2
  exit 2
fi
ROOT="$(cd "$ROOT" && pwd -P)"
toplevel="$(git -C "$ROOT" rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$toplevel" || "$(cd "$toplevel" && pwd -P)" != "$ROOT" ]]; then
  echo "ERROR: $ROOT is not the top level of a git work tree" >&2
  exit 2
fi

# A literal workflow expression, not a shell one.
# shellcheck disable=SC2016
READ_OUTPUT='${{ steps.terraform-version.outputs.version }}'
findings=()

# Rule 1.
if ! git -C "$ROOT" ls-files --error-unmatch -- .terraform-version >/dev/null 2>&1; then
  findings+=(".terraform-version: missing or not tracked by git, so CI's checkout would not have it")
elif ! msg="$(bash "${SCRIPT_DIR}/terraform-version.sh" "$ROOT" 2>&1 >/dev/null)"; then
  findings+=("${msg#::error::}")
fi

# count PATTERN FILE: fixed-string line matches, 0 when none.
count() {
  local n
  n="$(grep -c -F -- "$1" "$2" || true)"
  echo "${n:-0}"
}

total_setups=0
while IFS= read -r wf; do
  [[ -z "$wf" ]] && continue
  file="${ROOT}/${wf}"

  # Rule 2.
  while IFS= read -r hit; do
    [[ -n "$hit" ]] && findings+=("${wf}:${hit}  [TF_VERSION pin; read .terraform-version instead]")
  done < <(grep -n -F 'TF_VERSION' "$file" || true)

  # Rule 3. Strip a trailing comment before comparing.
  while IFS= read -r hit; do
    [[ -z "$hit" ]] && continue
    value="${hit#*terraform_version:}"
    value="${value%%#*}"
    value="$(echo "$value" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    if [[ "$value" != "$READ_OUTPUT" ]]; then
      findings+=("${wf}:${hit}  [terraform_version must be ${READ_OUTPUT}]")
    fi
  done < <(grep -n -E '^[[:space:]]*terraform_version:' "$file" || true)

  # Rule 4.
  setups="$(count 'hashicorp/setup-terraform@' "$file")"
  inputs="$(count "terraform_version: ${READ_OUTPUT}" "$file")"
  reads="$(count 'run: bash scripts/terraform-version.sh' "$file")"
  total_setups=$((total_setups + setups))
  if ! [[ "$setups" -eq "$inputs" && "$setups" -eq "$reads" ]]; then
    findings+=("${wf}: ${setups} setup-terraform step(s), ${inputs} terraform_version input(s) reading the step output, ${reads} 'bash scripts/terraform-version.sh' step(s); each setup needs one of each")
  fi
done < <(git -C "$ROOT" ls-files -- '.github/workflows/*.yml' '.github/workflows/*.yaml')

# Rule 5.
if [[ $total_setups -eq 0 ]]; then
  findings+=("no workflow uses hashicorp/setup-terraform; the scan found nothing to vouch for")
fi

if [[ ${#findings[@]} -gt 0 ]]; then
  {
    echo "FAILED: workflows must install the Terraform version in .terraform-version."
    echo ""
    printf '  %s\n' "${findings[@]}"
    echo ""
    echo "Each job reads it right before setup-terraform:"
    echo "  - name: Read Terraform version"
    echo "    id: terraform-version"
    echo "    run: bash scripts/terraform-version.sh"
    echo "  - uses: hashicorp/setup-terraform@<sha>"
    echo "    with:"
    echo "      terraform_version: ${READ_OUTPUT}"
  } >&2
  exit 1
fi

echo "OK: $(cat "${ROOT}/.terraform-version") is pinned only in .terraform-version; ${total_setups} setup-terraform step(s) read it."
