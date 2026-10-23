#!/usr/bin/env bash
# test-run-gosec.sh
#
# Self-tests for scripts/run-gosec.sh and the three callers that must share its
# rule set: the pre-commit hook (scripts/gosec-hook.sh), `make security-scan-go`
# and the CI gosec step (issue #340).
#
# The run-gosec.sh cases plant a fake gosec at the script's cache path (under a
# temporary XDG_CACHE_HOME) that logs "<dir>|<args>" per call. GOPROXY=off
# makes any attempt to install a real gosec fail fast instead of reaching the
# network.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
work="$(mktemp -d "${TMPDIR:-/tmp}/run-gosec-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

export GOTOOLCHAIN=local GOPROXY=off XDG_CACHE_HOME="$work/cache" HOME="$work/home"
mkdir -p "$HOME"
# The pin is read from .gosec-version, as run-gosec.sh does, so a version bump
# needs no test change. A missing pin is reported below; the other cases still
# run.
gosec_version="$(cat "$REPO_ROOT/.gosec-version" 2>/dev/null || true)"
fake_dir="$XDG_CACHE_HOME/gosec/${gosec_version}-$(go env GOVERSION)"
mkdir -p "$fake_dir"
cat >"$fake_dir/gosec" <<'FAKE'
#!/usr/bin/env bash
# Fake gosec: log the call, write the SARIF file unless told not to, and fail
# when the working directory ends in FAKE_FAIL_IN.
echo "$(pwd -P)|$*" >>"$FAKE_LOG"
prev=""
for a in "$@"; do
  if [[ "$prev" == "-out" && "${FAKE_NO_SARIF:-}" != "1" ]]; then echo '{"runs":[]}' >"$a"; fi
  prev="$a"
done
if [[ -n "${FAKE_FAIL_IN:-}" && "$(pwd -P)" == *"/$FAKE_FAIL_IN" ]]; then exit 1; fi
exit 0
FAKE
chmod +x "$fake_dir/gosec"
export FAKE_LOG="$work/calls.log"

fail=0
pass() { echo "PASS: $1"; }
failed() { echo "FAIL: $1"; fail=1; }

if [[ -n "$gosec_version" ]]; then
  pass ".gosec-version pins gosec ($gosec_version)"
else
  failed ".gosec-version pins gosec"
fi

# run LABEL WANT_RC CMD... : runs CMD with a fresh log, checks its exit code.
rc=0
run() {
  local label="$1" want="$2"
  shift 2
  : >"$FAKE_LOG"
  rc=0
  "$@" >"$work/out.txt" 2>&1 || rc=$?
  if [[ "$rc" -ne "$want" ]]; then
    failed "$label (exit $rc, want $want)"
    sed 's/^/    /' "$work/out.txt"
    return 1
  fi
}

# expect_calls LABEL LINE... : the log holds exactly these calls, in order.
expect_calls() {
  local label="$1"
  shift
  local want got
  want="$(printf '%s\n' "$@")"
  got="$(cat "$FAKE_LOG")"
  if [[ "$got" == "$want" ]]; then
    pass "$label"
  else
    failed "$label"
    printf '    want:\n%s\n    got:\n%s\n' "$want" "$got"
  fi
}

E2E="$REPO_ROOT/tests/e2e"
DIRS="-exclude-dir=.legacy -exclude-dir=.dev-notes -exclude-dir=vendor"

# --- run-gosec.sh ------------------------------------------------------------

if run "full scan" 0 bash "$SCRIPT_DIR/run-gosec.sh"; then
  expect_calls "full scan covers every go.work module with no -exclude" \
    "$REPO_ROOT|$DIRS -quiet ./..." \
    "$E2E|$DIRS -quiet ./..."
fi

if run "sarif scan" 0 bash "$SCRIPT_DIR/run-gosec.sh" --sarif-dir "$work/sarif"; then
  expect_calls "sarif scan covers every module with no -exclude" \
    "$REPO_ROOT|$DIRS -fmt sarif -out $work/sarif/gosec-root.sarif ./..." \
    "$E2E|$DIRS -fmt sarif -out $work/sarif/gosec-tests-e2e.sarif ./..."
  if [[ -f "$work/sarif/gosec-root.sarif" && -f "$work/sarif/gosec-tests-e2e.sarif" ]]; then
    pass "sarif files keep the gosec-root / gosec-tests-e2e names"
  else
    failed "sarif files keep the gosec-root / gosec-tests-e2e names"
  fi
fi

if FAKE_FAIL_IN="${REPO_ROOT##*/}" run "a finding in the root module fails the run" 1 \
  bash "$SCRIPT_DIR/run-gosec.sh"; then
  expect_calls "a finding in the root module still scans tests/e2e" \
    "$REPO_ROOT|$DIRS -quiet ./..." \
    "$E2E|$DIRS -quiet ./..."
fi

if FAKE_NO_SARIF=1 run "a module without SARIF output fails the run" 1 \
  bash "$SCRIPT_DIR/run-gosec.sh" --sarif-dir "$work/sarif2"; then
  pass "missing SARIF output is an error"
fi

if run "selected packages" 0 bash "$SCRIPT_DIR/run-gosec.sh" \
  --module . ./internal/api ./cmd/server --module tests/e2e .; then
  expect_calls "selected packages are scanned per module" \
    "$REPO_ROOT|$DIRS -quiet ./internal/api ./cmd/server" \
    "$E2E|$DIRS -quiet ."
fi

if run "whitespace package path" 2 bash "$SCRIPT_DIR/run-gosec.sh" --module . "./a b"; then
  pass "a package path with whitespace is a usage error"
fi

if run "unknown argument" 2 bash "$SCRIPT_DIR/run-gosec.sh" -exclude=G104; then
  pass "extra gosec flags such as -exclude are rejected"
fi

# --- pre-commit hook -----------------------------------------------------------

# Invoked through run (below), which shellcheck cannot see.
# shellcheck disable=SC2329
hook() { (cd "$REPO_ROOT" && bash scripts/gosec-hook.sh "$@"); }

if run "hook maps staged files" 0 hook internal/api/router.go internal/api/claimed_row_check.go \
  cmd/server/main.go tests/e2e/e2e_test.go scripts/testdata/none.go does/not/exist.go; then
  expect_calls "hook scans each changed package with the shared rule set" \
    "$REPO_ROOT|$DIRS -quiet ./cmd/server ./internal/api" \
    "$E2E|$DIRS -quiet ."
fi

if run "hook with only deleted files" 0 hook does/not/exist.go; then
  expect_calls "hook runs no scan when nothing survives filtering"
fi

if FAKE_FAIL_IN="${REPO_ROOT##*/}" run "hook fails on a finding" 1 hook internal/api/router.go; then
  pass "hook propagates gosec findings"
fi

# --- Makefile and CI ------------------------------------------------------------

make_cmds="$(make --no-print-directory -C "$REPO_ROOT" -n security-scan-go 2>&1)"
if [[ "$make_cmds" == "scripts/run-gosec.sh" ]]; then
  pass "make security-scan-go runs only scripts/run-gosec.sh"
else
  failed "make security-scan-go runs only scripts/run-gosec.sh (got: $make_cmds)"
fi

# The body of the CI step, up to the next step.
ci_step="$(awk '
  /^      - name: Run gosec Security Scanner$/ { on = 1; next }
  on && /^      - name: / { exit }
  on { print }
' "$REPO_ROOT/.github/workflows/ci.yml")"
ci_code="$(printf '%s\n' "$ci_step" | grep -v '^[[:space:]]*#' || true)"
# The literal text of the call, $RUNNER_TEMP included.
# shellcheck disable=SC2016
if [[ "$ci_code" == *'scripts/run-gosec.sh --sarif-dir "$RUNNER_TEMP"'* ]]; then
  pass "CI gosec step calls scripts/run-gosec.sh"
else
  failed "CI gosec step calls scripts/run-gosec.sh"
fi
# Messages may name gosec; commands may not (a direct call, an install or a
# rule-set flag would be a second rule set).
if printf '%s\n' "$ci_code" | grep -v 'echo ' |
  grep -Eq '(^[[:space:]]*|[;&|(][[:space:]]*)gosec[[:space:]]|securego/gosec|-exclude'; then
  failed "CI gosec step neither installs nor runs gosec itself"
else
  pass "CI gosec step neither installs nor runs gosec itself"
fi

# No other gosec rule set may creep back in next to the shared one.
stray="$(cd "$REPO_ROOT" && grep -nE -- '-exclude=G[0-9]|GOSEC_EXCLUDE' \
  Makefile .pre-commit-config.yaml .github/workflows/*.yml scripts/*.sh 2>/dev/null \
  | grep -vE '^scripts/(run-gosec|test-run-gosec)\.sh:' || true)"
if [[ -z "$stray" ]]; then
  pass "no gosec -exclude list outside run-gosec.sh"
else
  failed "no gosec -exclude list outside run-gosec.sh"
  printf '%s\n' "$stray" | sed 's/^/    /'
fi

# Nor may anything else run gosec itself (a `gosec -flag` or `gosec ./...` call
# in code, not in a comment).
direct="$(cd "$REPO_ROOT" && grep -nE -- '(^|[^-/[:alnum:]_.])gosec[[:space:]]+(-|\./)' \
  Makefile .github/workflows/*.yml scripts/*.sh 2>/dev/null \
  | grep -vE '^scripts/(run-gosec|test-run-gosec)\.sh:' \
  | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true)"
if [[ -z "$direct" ]]; then
  pass "nothing but run-gosec.sh runs gosec"
else
  failed "nothing but run-gosec.sh runs gosec"
  printf '%s\n' "$direct" | sed 's/^/    /'
fi

exit "$fail"
