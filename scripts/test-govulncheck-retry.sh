#!/usr/bin/env bash
# test-govulncheck-retry.sh
#
# Runs govulncheck-retry.sh against a fake govulncheck on PATH. The fake reads
# a comma-separated script of "exit[:stderr]" steps from FAKE_STEPS, one per
# call, repeating the last step, and counts calls in FAKE_COUNT_FILE.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/govulncheck-retry-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

cat >"$work/govulncheck" <<'FAKE'
#!/usr/bin/env bash
n=$(($(cat "$FAKE_COUNT_FILE" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE_COUNT_FILE"
IFS='|' read -r -a steps <<<"$FAKE_STEPS"
i=$((n - 1))
((i < ${#steps[@]})) || i=$((${#steps[@]} - 1))
step="${steps[$i]}"
[[ "$step" == *:* ]] && echo "${step#*:}" >&2
exit "${step%%:*}"
FAKE
chmod +x "$work/govulncheck"

RESET="govulncheck: fetching vulnerabilities: Get https://vuln.go.dev/index/db.json: read tcp: connection reset by peer"
export FAKE_COUNT_FILE="$work/count" GOVULNCHECK_RETRY_SLEEP=0
fail=0

# run_case LABEL STEPS EXPECTED_EXIT EXPECTED_CALLS
run_case() {
  local label="$1" steps="$2" want_rc="$3" want_calls="$4" rc=0 calls
  rm -f "$FAKE_COUNT_FILE"
  FAKE_STEPS="$steps" PATH="$work:$PATH" bash "$SCRIPT_DIR/govulncheck-retry.sh" ./... >/dev/null 2>&1 || rc=$?
  calls="$(cat "$FAKE_COUNT_FILE")"
  if [[ "$rc" -eq "$want_rc" && "$calls" -eq "$want_calls" ]]; then
    echo "PASS: $label"
  else
    echo "FAIL: $label (exit $rc want $want_rc, calls $calls want $want_calls)"
    fail=1
  fi
}

run_case "vulnerabilities found (exit 3) are not retried" "3:$RESET|0" 3 1
run_case "transient fetch failures then success pass" "1:$RESET|1:$RESET|0" 0 3
run_case "persistent transient failure fails after 3 attempts" "1:$RESET" 1 3
run_case "non-network error is not retried" "1:govulncheck: loading packages: no go.mod" 1 1
run_case "fetch error without a network cause is not retried" "1:govulncheck: fetching vulnerabilities: invalid JSON" 1 1
run_case "network token without fetch text is not retried" "1:dial tcp: i/o timeout" 1 1
run_case "exit 2 with fetch text is not retried" "2:$RESET" 2 1

exit "$fail"
