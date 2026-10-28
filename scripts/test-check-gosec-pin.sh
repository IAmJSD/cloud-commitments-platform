#!/usr/bin/env bash
# test-check-gosec-pin.sh
#
# Exercises check-gosec-pin.sh in BOTH directions: a tree that pins gosec only
# in .gosec-version must pass, and each way of hard-coding the version again
# must fail. A guard that only ever returns "clean" is indistinguishable from a
# broken one, so the failing cases are the ones that give this check its value.
#
# It also asserts that the Makefile and scripts/run-gosec.sh (called by the
# pre-commit hook, `make security-scan-go` and CI) really read .gosec-version (a
# changed file changes what they install, and a malformed one stops them), so
# the readers cannot quietly go back to a literal.
#
# Fixtures are throwaway git repos under a temp dir; nothing touches the real
# tree. This file is exempt from the guard because it spells out the patterns.
#
# Exits 0 when all cases pass; exits 1 on any failure.

# Fixture contents are literal text (`${{ }}`, `$(...)`, `${V}`) that must reach
# the files unexpanded, so single quotes are intended throughout.
# shellcheck disable=SC2016

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHECK="${SCRIPT_DIR}/check-gosec-pin.sh"

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

# A minimal tree that pins gosec correctly: the version file plus one install
# site that reads it.
n=0
new_repo() {
  n=$((n + 1))
  local dir="${WORK}/repo-${n}"
  mkdir -p "${dir}/.github/workflows"
  git -C "$dir" init -q >/dev/null 2>&1
  printf 'v2.28.0\n' > "${dir}/.gosec-version"
  cat > "${dir}/.github/workflows/ci.yml" <<'EOF'
jobs:
  scan:
    steps:
      - run: |
          gosec_version="$(cat .gosec-version)"
          go install "github.com/securego/gosec/v2/cmd/gosec@${gosec_version}"
EOF
  REPO="$dir"
}

track() { git -C "$REPO" add -A; }

# add_file PATH CONTENT: write a file into the current fixture and track it.
add_file() {
  mkdir -p "$(dirname "${REPO}/$1")"
  printf '%s\n' "$2" > "${REPO}/$1"
  track
}

# ---- clean trees ------------------------------------------------------------

new_repo; track
run_case "a tree that pins gosec only in .gosec-version exits 0" 0 "$REPO"

new_repo
add_file CHANGELOG.md "- ci: bump gosec to v2.28.0"
add_file docs/audits/audit.md 'the CI step runs `gosec@v2.28.0`'
run_case "CHANGELOG.md and docs/audits/ may name historical versions" 0 "$REPO"

new_repo
add_file Makefile 'GOSEC_VERSION_FILE:=.gosec-version
GOSEC_VERSION?=$(shell cat $(GOSEC_VERSION_FILE))
install:
	go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)'
run_case "a Makefile install site that reads .gosec-version exits 0" 0 "$REPO"

# Neighbors of the pin that are not gosec versions must not be reported.
new_repo
add_file .github/workflows/pre-commit.yml '      - run: go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0
          key: go-tools-${{ runner.os }}-gocyclo-v0.6.0
          sarif_file: ${{ runner.temp }}/gosec-results.sarif
        uses: github/codeql-action/upload-sarif@7211b7c8 # v4.36.0
          # gosec v2 rules, see G104'
run_case "other tools' pins and gosec mentions without a version exit 0" 0 "$REPO"

# ---- hard-coded versions ----------------------------------------------------

literal_case() {
  local label="$1" path="$2" content="$3"
  new_repo
  add_file "$path" "$content"
  run_case "$label exits 1" 1 "$REPO"
}

literal_case "a literal install ref (gosec@v2.28.0)" .github/workflows/x.yml \
  '# reads .gosec-version
        run: go install github.com/securego/gosec/v2/cmd/gosec@v2.28.0'
literal_case "a Makefile assignment (GOSEC_VERSION?=v2.28.0)" Makefile \
  'GOSEC_VERSION?=v2.28.0'
literal_case "a shell assignment (GOSEC_VERSION=\"2.28.0\")" scripts/hook.sh \
  'GOSEC_VERSION="2.28.0"'
literal_case "a YAML env value (GOSEC_VERSION: 'v2.28.0')" .github/workflows/x.yml \
  "      GOSEC_VERSION: 'v2.28.0'"
literal_case "a cache key (gosec-v2.28.0)" .github/workflows/x.yml \
  '          key: go-tools-${{ runner.os }}-gosec-v2.28.0-gocyclo-v0.6.0'
literal_case "a cache path (pre-commit-gosec/v2.28.0/)" .pre-commit-config.yaml \
  '        # ~/.cache/pre-commit-gosec/v2.28.0/gosec on first use.'
literal_case "a prose comment (runs gosec v2.28.0)" .github/workflows/x.yml \
  '          # the Security Scanning job runs gosec v2.28.0 per module'
literal_case "a prose comment (gosec version 2.28.0)" docs/DEVELOPMENT.md \
  'CI uses gosec version 2.28.0.'
literal_case "a mixed-case, unprefixed mention (GoSec 2.28.0)" README.md \
  'GoSec 2.28.0 is what CI runs'
literal_case "an image tag (securego/gosec:2.28.0)" Dockerfile \
  'FROM securego/gosec:2.28.0'
literal_case "a go.mod tool requirement" go.mod \
  'require github.com/securego/gosec/v2 v2.28.0'
literal_case "a docs/ file outside docs/audits/" docs/notes.md \
  'run `gosec@v2.28.0` locally'

# ---- refs that bypass the file ----------------------------------------------

literal_case "an unpinned install (gosec@latest)" scripts/x.sh \
  '# .gosec-version
echo "Install: go install github.com/securego/gosec/v2/cmd/gosec@latest"'
literal_case "a GitHub Action ref (securego/gosec@<sha>)" .github/workflows/x.yml \
  '        uses: securego/gosec@6decf96c3d272d5a8bbdcf9fddb5789d0be16a8d'

literal_case "an install site that never mentions .gosec-version" scripts/x.sh \
  'GOBIN=/tmp go install "github.com/securego/gosec/v2/cmd/gosec@${V}"'
literal_case "a continued install line that never mentions .gosec-version" scripts/x.sh \
  'GOBIN=/tmp go install \
    "github.com/securego/gosec/v2/cmd/gosec@${V}"'

new_repo
rm "${REPO}/.github/workflows/ci.yml"
track
run_case "a tree with no gosec install site at all exits 1" 1 "$REPO"

# ---- the version file itself ------------------------------------------------

new_repo
rm "${REPO}/.gosec-version"
track
run_case "a missing .gosec-version exits 1" 1 "$REPO"

new_repo
track
git -C "$REPO" rm -q --cached .gosec-version
run_case "an untracked .gosec-version exits 1" 1 "$REPO"

for bad in '2.28.0' 'v2.28' 'latest' '' 'v2.28.0 # pinned' $'v2.28.0\nv2.29.0' $'v2.28.0\r'; do
  new_repo
  printf '%s\n' "$bad" > "${REPO}/.gosec-version"
  track
  run_case "a malformed .gosec-version ($(printf '%q' "$bad")) exits 1" 1 "$REPO"
done

# ---- usage errors -----------------------------------------------------------

new_repo; track
mkdir -p "${REPO}/sub"
run_case "unknown flag exits 2" 2 --bogus
run_case "two arguments exit 2" 2 "$REPO" "$REPO"
run_case "a missing directory exits 2" 2 "${WORK}/does-not-exist"
run_case "a directory below the work tree top level exits 2" 2 "${REPO}/sub"
mkdir -p "${WORK}/not-a-repo"
run_case "a directory that is not a git work tree exits 2" 2 "${WORK}/not-a-repo"

# ---- the readers really read the file ---------------------------------------

# The Makefile: a different version in the file must change what
# install-dev-tools installs. A literal in the Makefile would not follow it.
if command -v make >/dev/null 2>&1; then
  mk="${WORK}/makefile-reader"
  mkdir -p "$mk"
  cp "${REPO_ROOT}/Makefile" "${mk}/Makefile"
  printf 'v9.8.7\n' > "${mk}/.gosec-version"
  actual=1
  # GOSEC_VERSION is unset so the Makefile's own default applies. The output is
  # captured first: `grep -q` closing the pipe early would SIGPIPE make and,
  # under pipefail, fail the case for the wrong reason.
  recipe="$(cd "$mk" && env -u GOSEC_VERSION make -s -n install-dev-tools 2>/dev/null || true)"
  if grep -q -F 'github.com/securego/gosec/v2/cmd/gosec@v9.8.7' <<< "$recipe"; then
    actual=0
  fi
  record "make install-dev-tools installs the version in .gosec-version" 0 "$actual"
else
  echo "FAIL: make is not installed, cannot check the Makefile reader"
  (( fail++ )) || true
fi

# scripts/run-gosec.sh, which the pre-commit hook, `make security-scan-go` and
# CI all call: a missing or malformed file must stop it (exit 2) before it runs
# Go or touches its cache. HOME is redirected so no real cache is touched, and
# `go` is a stub that only records how it was called (it answers
# `go env GOVERSION` and fails everything else), so a script that ignores the
# file fails here quickly instead of downloading gosec.
reader_n=0
# reader_fixture CONTENT: build a fixture repo; sets READER_DIR.
reader_fixture() {
  local content="$1"
  reader_n=$((reader_n + 1))
  local dir="${WORK}/reader-${reader_n}"
  mkdir -p "${dir}/scripts" "${dir}/home" "${dir}/bin"
  git -C "$dir" init -q >/dev/null 2>&1
  cp "${SCRIPT_DIR}/run-gosec.sh" "${SCRIPT_DIR}/gosec-hook.sh" "${dir}/scripts/"
  printf 'package main\n' > "${dir}/main.go"
  if [[ "$content" != "<missing>" ]]; then
    printf '%s\n' "$content" > "${dir}/.gosec-version"
  fi
  printf '#!/bin/sh\necho "GOBIN=$GOBIN $*" >> "%s"\n[ "$1 $2" = "env GOVERSION" ] && { echo go1.26.9; exit 0; }\nexit 1\n' \
    "${dir}/go-calls" > "${dir}/bin/go"
  chmod +x "${dir}/bin/go"
  READER_DIR="$dir"
}

# run_reader SCRIPT ARGS...: run a copied script in the fixture; sets READER_EXIT.
run_reader() {
  READER_EXIT=0
  (cd "$READER_DIR" && env -u XDG_CACHE_HOME HOME="${READER_DIR}/home" \
      PATH="${READER_DIR}/bin:${PATH}" GOTOOLCHAIN=local \
      bash "$@" >/dev/null 2>&1) || READER_EXIT=$?
}

# reader_reject_case LABEL CONTENT SCRIPT ARGS...
reader_reject_case() {
  local label="$1" content="$2"
  shift 2
  reader_fixture "$content"
  run_reader "$@"
  local dir="$READER_DIR" actual="$READER_EXIT"
  if [[ -e "${dir}/go-calls" || -e "${dir}/home/.cache" ]]; then
    echo "FAIL: $label (go ran or the cache was created before the version was validated)"
    (( fail++ )) || true
    return
  fi
  record "$label" 2 "$actual"
}
reader_reject_case "run-gosec.sh stops with exit 2 when .gosec-version is missing" \
  "<missing>" scripts/run-gosec.sh
reader_reject_case "run-gosec.sh stops with exit 2 on an unprefixed version (2.28.0)" \
  "2.28.0" scripts/run-gosec.sh
reader_reject_case "run-gosec.sh stops with exit 2 on a non-version (latest)" \
  "latest" scripts/run-gosec.sh
reader_reject_case "the pre-commit hook stops with exit 2 when .gosec-version is missing" \
  "<missing>" scripts/gosec-hook.sh main.go

# reader_install_case LABEL SCRIPT ARGS...: with a valid file the script must
# install exactly that version, into a private dir under the gosec cache. The
# stub `go` fails the install, so the script then exits 2; what matters is the
# recorded call.
reader_install_case() {
  local label="$1"
  shift
  reader_fixture "v9.8.7"
  run_reader "$@"
  local want_re="^GOBIN=${READER_DIR}/home/\.cache/gosec/\.install\.[^ /]+ install github\.com/securego/gosec/v2/cmd/gosec@v9\.8\.7\$"
  if [[ -f "${READER_DIR}/go-calls" ]] && grep -q -E "$want_re" "${READER_DIR}/go-calls"; then
    record "$label" 0 0
  else
    echo "FAIL: $label (go calls: $(cat "${READER_DIR}/go-calls" 2>/dev/null || echo none))"
    (( fail++ )) || true
  fi
}
reader_install_case "run-gosec.sh installs the version in .gosec-version" scripts/run-gosec.sh
reader_install_case "the pre-commit hook installs the version in .gosec-version" \
  scripts/gosec-hook.sh main.go

# ---- the real tree ----------------------------------------------------------

run_case "this repository pins gosec only in .gosec-version" 0

echo ""
echo "Results: ${pass} passed, ${fail} failed."
[[ "$fail" -eq 0 ]]
