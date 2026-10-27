#!/usr/bin/env bash
# check-gosec-pin.sh
#
# Fails when the gosec version is written anywhere other than .gosec-version.
#
# .gosec-version (repo root, one line such as `v2.28.0`) is the single source of
# truth for the gosec version. scripts/run-gosec.sh (which ci.yml's Security
# Scanning job, `make security-scan-go` and the pre-commit hook all call) and
# the Makefile's install-dev-tools target read it. Before issue #347 the same
# version was hard-coded in four places (ci.yml, pre-commit.yml, the Makefile
# and gosec-hook.sh) plus comments that restated it, so a bump only had to miss
# one of them for local scans and the CI scan that uploads to GitHub Security
# to run different rule sets.
#
# Every tracked text file is scanned (git grep), not a list of known pin sites,
# so a new workflow or script that hard-codes the version is caught without
# anyone remembering to add it here. Findings:
#
#   1. .gosec-version is missing, untracked, or not exactly `vMAJOR.MINOR.PATCH`.
#   2. A version literal written next to "gosec" (any case): an install ref
#      (`gosec@v2.28.0`), an assignment (`GOSEC_VERSION?=v2.28.0`,
#      `GOSEC_VERSION="2.28.0"`), a cache key (`gosec-v2.28.0`), a path
#      (`pre-commit-gosec/v2.28.0/`), an image tag (`securego/gosec:2.28.0`),
#      a go.mod requirement, or prose (`runs gosec v2.28.0`). Prose counts too:
#      a comment that restates the version goes stale on the next bump.
#   3. A gosec install or action ref that names anything other than a variable,
#      such as `gosec@latest`, `gosec@master` or `securego/gosec@<sha>`.
#   4. An install site that does not mention .gosec-version, i.e. one that
#      takes its version from somewhere else. An install site is any file that
#      names the gosec command at a version (`securego/gosec/v2/cmd/gosec@`, as
#      `go install` and `go run` do; matched per line, so a `go install \`
#      continued onto the next line still counts). At least one must exist, so
#      a scan that silently matched nothing cannot pass.
#
# Not scanned: CHANGELOG.md and docs/audits/, which record history and may name
# the versions that were current at the time, and this script and its self-test
# (scripts/test-check-gosec-pin.sh), which spell out the patterns.
#
# LIMITATIONS. A version stored in a variable whose name does not contain
# "gosec" (e.g. `V=2.28.0` then `gosec@$V`) is not caught by rule 2; rule 4
# still requires that file to mention .gosec-version.
#
# Usage: check-gosec-pin.sh [ROOT]
#   ROOT defaults to this repository. It must be the top level of a git work
#   tree; only tracked files are scanned. The self-test points it at fixtures.
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

ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
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

VERSION_FILE=".gosec-version"
# The guard and its self-test spell out the patterns they look for.
EXCLUDES=(
  ":(exclude)CHANGELOG.md"
  ":(exclude)docs/audits/**"
  ":(exclude)scripts/check-gosec-pin.sh"
  ":(exclude)scripts/test-check-gosec-pin.sh"
)

# Rule 2: "gosec", then any non-space run (e.g. `_VERSION`, `/v2/cmd/gosec`),
# an optional word "version", separators, then a version literal.
LITERAL_RE="gosec[^[:space:]]*([[:space:]]+version)?[\"'@:=?/_[:space:]-]+v?[0-9]+\.[0-9]+\.[0-9]+"
# Rule 3: a gosec ref whose value after `@` does not start with `$`.
REF_RE="gosec[^[:space:]@]*@[\"']?[^$\"'[:space:]]"
# Rule 4: an install site.
INSTALL_RE="securego/gosec(/v[0-9]+)?/cmd/gosec@"

findings=()

# git grep exits 1 for "no match"; anything above that is a real error.
grep_tree() {
  local rc=0 out
  out="$(git -C "$ROOT" grep "$@" -- . "${EXCLUDES[@]}")" || rc=$?
  if [[ $rc -gt 1 ]]; then
    echo "ERROR: git grep failed (exit $rc) in $ROOT" >&2
    exit 2
  fi
  # $(...) strips the final newline; put it back so `read` sees the last line.
  [[ -n "$out" ]] && printf '%s\n' "$out"
  return 0
}

# Rule 1.
if [[ ! -f "$ROOT/$VERSION_FILE" ]]; then
  findings+=("$VERSION_FILE: missing")
elif ! git -C "$ROOT" ls-files --error-unmatch -- "$VERSION_FILE" >/dev/null 2>&1; then
  findings+=("$VERSION_FILE: not tracked by git, so CI's checkout would not have it")
else
  version="$(cat "$ROOT/$VERSION_FILE")"
  if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    findings+=("$VERSION_FILE: must be one line of the form vMAJOR.MINOR.PATCH, got '${version}'")
  fi
fi

# Rules 2 and 3.
while IFS= read -r hit; do
  [[ -n "$hit" ]] && findings+=("$hit  [hard-coded gosec version]")
done < <(grep_tree -n -I -i -E "$LITERAL_RE")
while IFS= read -r hit; do
  [[ -n "$hit" ]] && findings+=("$hit  [gosec ref not read from $VERSION_FILE]")
done < <(grep_tree -n -I -i -E "$REF_RE")

# Rule 4.
install_sites=0
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  install_sites=$((install_sites + 1))
  if ! git -C "$ROOT" grep -q -F "$VERSION_FILE" -- "$f"; then
    findings+=("$f: installs gosec but never mentions $VERSION_FILE")
  fi
done < <(grep_tree -l -I -i -E "$INSTALL_RE")
if [[ $install_sites -eq 0 ]]; then
  findings+=("no file names securego/gosec/v2/cmd/gosec@...; the scan found no install site, so it cannot vouch for any")
fi

if [[ ${#findings[@]} -gt 0 ]]; then
  {
    echo "FAILED: the gosec version must be written only in $VERSION_FILE."
    echo ""
    printf '  %s\n' "${findings[@]}"
    echo ""
    echo "To bump gosec, edit $VERSION_FILE and nothing else. Install sites read it:"
    echo "  go install \"github.com/securego/gosec/v2/cmd/gosec@\$(cat $VERSION_FILE)\""
    echo "See the header of scripts/check-gosec-pin.sh for what is checked."
  } >&2
  exit 1
fi

echo "OK: gosec ${version} is pinned only in $VERSION_FILE; ${install_sites} install site(s) read it."
