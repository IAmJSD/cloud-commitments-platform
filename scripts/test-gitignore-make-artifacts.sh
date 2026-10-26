#!/usr/bin/env bash
# test-gitignore-make-artifacts.sh
#
# Asserts that the files the documented Make targets write at the repo root are
# ignored by the committed .gitignore (#279), so `git add -A` after a build,
# coverage run or scan cannot pick them up. The worst case is the multi-MB
# `bootstrap` Lambda binary from `make build-lambda`; binaries of that size have
# reached branch history before.
#
# Both directions are asserted:
#   - each root artifact IS ignored, and the match comes from .gitignore itself
#     (not from a developer's global excludes file or .git/info/exclude, which
#     would let this pass locally on a checkout CI does not have);
#   - the same file name one directory down is NOT ignored, so the patterns
#     stay anchored to the root and cannot hide a tracked file or fixture of
#     the same name elsewhere in the tree.
#
# Each artifact name must also still appear in the Makefile as an output: right
# after `-o`, `--out`, `--output`, `tee` or a `>`/`>>` redirection. If a target
# is renamed or stops writing the file, this fails rather than passing on a
# pattern that no longer guards anything. Other mentions do not count: the
# `make clean` rm lines can outlive the target that wrote the file (as
# gosec-report.json did once `make security-scan-go` moved to
# scripts/run-gosec.sh, #340), and the `@echo "...: <name>"` status lines can
# outlive a renamed output.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Root artifact -> the Make target that writes it.
ARTIFACTS=(
  "bootstrap:build-lambda"
  "coverage.html:test-coverage"
  "trivy-report.json:security-scan-docker"
  "tfsec-report.json:security-scan-terraform"
  "complexity-report.txt:complexity-report"
)

pass=0
fail=0

ok() {
  echo "PASS: $1"
  (( pass++ )) || true
}

not_ok() {
  echo "FAIL: $1"
  (( fail++ )) || true
}

# Prints the .gitignore rule that ignores $1, or nothing. --no-index makes the
# answer independent of whether the path happens to be tracked or staged.
gitignore_rule_for() {
  local verdict
  verdict="$(git -C "$REPO_ROOT" check-ignore -v --no-index -- "$1" 2>/dev/null || true)"
  # Output shape: <source>:<line>:<pattern><TAB><path>
  if [[ "$verdict" == .gitignore:* ]]; then
    printf '%s\n' "${verdict%%$'\t'*}"
  fi
}

# True when the Makefile writes $1: the name follows an output flag, `tee` or a
# redirection, and ends at whitespace, `;`, a quote or the end of the line.
makefile_writes() {
  local name_re="${1//./\\.}"
  grep -qE "((^|[[:space:]])(-o|--out|--output|tee)([[:space:]]+|=)|>>?[[:space:]]*)${name_re}([[:space:];\"']|$)" \
    "${REPO_ROOT}/Makefile"
}

for entry in "${ARTIFACTS[@]}"; do
  name="${entry%%:*}"
  target="${entry#*:}"

  if makefile_writes "$name"; then
    ok "Makefile still writes ${name} (make ${target})"
  else
    not_ok "Makefile no longer writes ${name} (no -o/--out/--output/tee/redirection names it); update this test and .gitignore together"
  fi

  rule="$(gitignore_rule_for "$name")"
  if [[ -n "$rule" ]]; then
    ok "root ${name} is ignored by ${rule}"
  else
    not_ok "root ${name} (make ${target}) is not ignored by .gitignore"
  fi

  nested="some/nested/dir/${name}"
  rule="$(gitignore_rule_for "$nested")"
  if [[ -z "$rule" ]]; then
    ok "nested ${nested} is not ignored (pattern is root-anchored)"
  else
    not_ok "nested ${nested} is ignored by ${rule}; anchor the pattern with a leading /"
  fi
done

echo ""
echo "Results: ${pass} passed, ${fail} failed"
if [[ "$fail" -gt 0 ]]; then
  exit 1
fi
