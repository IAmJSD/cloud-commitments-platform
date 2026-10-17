#!/usr/bin/env bash
# test-wait-for-healthy.sh
#
# Offline test for wait-for-healthy.sh: curl is stubbed via PATH. Also replays
# the pre-#488 inline smoke loop from deploy-azure.yml against the rollout-race
# scenario to show it passed there — the admission bug this changeset fixes.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUT="${SCRIPT_DIR}/wait-for-healthy.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# Stub curl. Each health request consumes the next token from $SCENARIO; the
# last token repeats forever.
#   H = HTTP 200 healthy
#   D = HTTP 200 but degraded body: what /health returned pre-#488, which the
#       pre-fix loop caught only by parsing the body
#   U = HTTP 503 with the degraded body: what /ready returns
#   M = HTTP 301 to https://app.example/ready with an empty body: what an
#       http:// URL gets from an HTTPS-only ALB listener (#647)
#   R = connection refused
# Models the real curl flags the callers use: -o FILE, -w with %{http_code} and
# %{redirect_url}, -f/--fail (exit 22 on >= 400, no body) and --fail-with-body
# (exit 22 on >= 400, body still written). Without either, curl exits 0 for any
# HTTP status, including 3xx and 5xx.
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
fwb=0
fail=0
out=""
wfmt=""
prev=""
for a in "$@"; do
  case "$prev" in
    -o) out="$a"; prev=""; continue ;;
    -w) wfmt="$a"; prev=""; continue ;;
  esac
  case "$a" in
    -o|-w) prev="$a"; continue ;;
    --fail-with-body) fwb=1 ;;
    --fail) fail=1 ;;
    --*) ;;
    # -f inside a short cluster such as -fsS.
    -*f*) fail=1 ;;
  esac
  url="$a"
done
# Real curl (7.76+) rejects -f/--fail together with --fail-with-body.
if [ "$fwb" = 1 ] && [ "$fail" = 1 ]; then
  echo "curl: option --fail-with-body: is badly used here" >&2
  exit 2
fi
# The pre-fix loop's warm-up request to /api/ succeeded silently and consumed no
# scenario token; only the gate request did.
case "$url" in
  */api/*) exit 0 ;;
esac
n=$(cat "$STATE" 2>/dev/null || echo 0)
tokens=($SCENARIO)
i=$n
((i >= ${#tokens[@]})) && i=$((${#tokens[@]} - 1))
echo $((n + 1)) > "$STATE"
degraded='{"status":"degraded","checks":{"auth_store":{"status":"unhealthy","message":"auth store not initialized"}}}'
location=""
case "${tokens[$i]}" in
  H) code=200; body='{"status":"healthy","checks":{}}' ;;
  D) code=200; body="$degraded" ;;
  U) code=503; body="$degraded" ;;
  M) code=301; body=""; location="https://app.example/ready" ;;
  R) echo "curl: (7) Failed to connect" >&2
     [ -n "$wfmt" ] && printf '000 '
     exit 7 ;;
esac
if ((code >= 400)) && [ "$fail" = 1 ]; then
  echo "curl: (22) The requested URL returned error: $code" >&2
  exit 22
fi
if [ -n "$body" ]; then
  if [ -n "$out" ]; then echo "$body" > "$out"; else echo "$body"; fi
fi
if [ -n "$wfmt" ]; then
  w="${wfmt//'%{http_code}'/$code}"
  printf '%s' "${w//'%{redirect_url}'/$location}"
fi
if ((code >= 400)) && [ "$fwb" = 1 ]; then
  echo "curl: (22) The requested URL returned error: $code" >&2
  exit 22
fi
exit 0
STUB
chmod +x "$WORK/bin/curl"

pass=0
fail=0

# run_case LABEL EXPECTED_EXIT SCENARIO [OUTPUT_SUBSTRING]
run_case() {
  local label="$1" expected="$2" scenario="$3" needle="${4:-}"
  local out actual=0
  : > "$WORK/state"
  out=$(PATH="$WORK/bin:$PATH" STATE="$WORK/state" SCENARIO="$scenario" \
    HEALTH_INTERVAL_SECONDS=0 HEALTH_MAX_ATTEMPTS=8 HEALTH_REQUIRED_STREAK=3 \
    "$SUT" https://app.example 2>&1) || actual=$?
  if [[ "$actual" -ne "$expected" ]]; then
    echo "FAIL: $label (expected exit $expected, got $actual)"; echo "$out"
    ((fail++)) || true
  elif [[ -n "$needle" && "$out" != *"$needle"* ]]; then
    echo "FAIL: $label (output lacks '$needle')"; echo "$out"
    ((fail++)) || true
  else
    echo "PASS: $label"
    ((pass++)) || true
  fi
}

run_case "not ready then ready x3 passes" 0 "U H H H"
run_case "old revision ready once, then not ready, then ready x3 passes" 0 "H U U H H H"
run_case "not ready in the middle of a streak resets it" 1 "H H U H H U" "auth store not initialized"
run_case "ready once then not ready forever fails with the degraded body" 1 "H U" "auth store not initialized"
run_case "connection refused forever fails with curl error" 1 "R" "Failed to connect"

run_case "curl error mid-streak resets it (H R H H U, streak 3, must not pass)" 1 "H R H H U" "Failed to connect"

# #647: an http:// URL in front of an HTTPS-only listener answers 301 on every
# path. Counting that as ready made the gate pass without reaching the app.
run_case "301 redirect forever fails and names the redirect target" 1 "M" "HTTP 301 redirect to https://app.example/ready"
run_case "301 mid-streak resets it (H H M H H U, must not pass)" 1 "H H M H H U" "auth store not initialized"
run_case "503 failure keeps the status and the body" 1 "U" "HTTP 503: {\"status\":\"degraded\""

# run_env_case LABEL EXPECTED_EXIT SCENARIO OUTPUT_SUBSTRING [ENV=VAL ...]
# Runs the SUT with ONLY the given env overrides (plus a zero sleep interval),
# so the script's own defaults (streak 3, 30 attempts) are exercised.
run_env_case() {
  local label="$1" expected="$2" scenario="$3" needle="$4"
  shift 4
  local out actual=0
  : > "$WORK/state"
  out=$(env PATH="$WORK/bin:$PATH" STATE="$WORK/state" SCENARIO="$scenario" \
    HEALTH_INTERVAL_SECONDS=0 "$@" "$SUT" https://app.example 2>&1) || actual=$?
  if [[ "$actual" -ne "$expected" ]]; then
    echo "FAIL: $label (expected exit $expected, got $actual)"; echo "$out"
    ((fail++)) || true
  elif [[ -n "$needle" && "$out" != *"$needle"* ]]; then
    echo "FAIL: $label (output lacks '$needle')"; echo "$out"
    ((fail++)) || true
  else
    echo "PASS: $label"
    ((pass++)) || true
  fi
}

run_env_case "defaults: H H U H H H passes only at attempt 6 (streak 3)" 0 "H H U H H H" "Readiness check 6: ready (3/3)"
run_env_case "defaults: two ready then not ready forever exhausts 30 attempts" 1 "H H U" "after 30 attempts"
run_env_case "streak 0 rejected" 2 "H" "HEALTH_REQUIRED_STREAK must be a positive integer" HEALTH_REQUIRED_STREAK=0
run_env_case "streak abc rejected" 2 "H" "HEALTH_REQUIRED_STREAK must be a positive integer" HEALTH_REQUIRED_STREAK=abc
run_env_case "streak empty rejected" 2 "H" "HEALTH_REQUIRED_STREAK must be a positive integer" HEALTH_REQUIRED_STREAK=
run_env_case "attempts 0 rejected" 2 "H" "HEALTH_MAX_ATTEMPTS must be a positive integer" HEALTH_MAX_ATTEMPTS=0
run_env_case "attempts abc rejected" 2 "H" "HEALTH_MAX_ATTEMPTS must be a positive integer" HEALTH_MAX_ATTEMPTS=abc
run_env_case "attempts empty rejected" 2 "H" "HEALTH_MAX_ATTEMPTS must be a positive integer" HEALTH_MAX_ATTEMPTS=
run_env_case "attempts below streak rejected" 2 "H" "could never pass" HEALTH_MAX_ATTEMPTS=2 HEALTH_REQUIRED_STREAK=3

# The pre-#488 loop from deploy-azure.yml, faithful to the original semantics:
# warm up on /api/ to trigger lazy DB init, gate on the /health BODY, and pass
# on the FIRST healthy response. D is the pre-fix /health contract: HTTP 200
# with a degraded body. This is the logic that admitted a cold replica: the
# warm-up initializes whichever replica the load balancer picked, and the first
# healthy answer ends the loop before a cold replica's degraded answer is seen.
old_inline() {
  local URL=https://app.example RESPONSE i
  for i in {1..3}; do
    curl -s -o /dev/null --max-time 60 "${URL}/api/auth/check-admin" || true
    if RESPONSE=$(curl -fsS --max-time 10 "$URL/health") &&
      jq -e '.status == "healthy"' > /dev/null <<< "$RESPONSE"; then
      echo "Health check $i: passed"
      return 0
    fi
    echo "Health check $i: not healthy yet: ${RESPONSE:-no response}"
    sleep 0
  done
  return 1
}
: > "$WORK/state"
# H D H H models the rollout race: the warmed replica answers the first probe,
# a still-cold replica would answer the second. Passing on the first H is
# exactly the #488 admission bug, so the pre-fix loop MUST pass here.
if PATH="$WORK/bin:$PATH" STATE="$WORK/state" SCENARIO="H D H H" old_inline >/dev/null 2>&1; then
  echo "PASS: pre-fix inline loop passes the rollout-race scenario (H D H H) on the first healthy response - the admission bug being fixed"
  ((pass++)) || true
else
  echo "FAIL: pre-fix inline loop unexpectedly failed the rollout-race scenario"
  ((fail++)) || true
fi

# The current gate must reject that same premature pass: a not-ready replica
# resets the streak, so the gate only passes once answers are CONSISTENTLY ready.
run_case "rollout race H U H H H passes under the readiness gate" 0 "H U H H H" "Readiness check 5: ready (3/3)"

# Real curl against a throwaway localhost server: the stub above cannot catch
# flag combinations that the installed curl rejects. Skipped when python3 is
# missing; curl older than 7.76 has no --fail-with-body and would fail here.
if command -v python3 > /dev/null; then
  # Three servers: one always answers 200, one 503 with a degraded body, and one
  # 301 to https on every path (an HTTPS-only ALB's port 80 listener, #647).
  python3 - > "$WORK/port" <<'PY' &
import http.server, sys, threading

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.server.mode == "redirect":
            self.send_response(301)
            self.send_header("Location", "https://app.example.invalid" + self.path)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        code = 200 if self.server.mode == "ok" else 503
        body = b'{"status":"degraded","checks":{"database":{"status":"unhealthy"}}}' if code == 503 else b'{"status":"healthy","checks":{}}'
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

servers = []
for mode in ("ok", "unready", "redirect"):
    s = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    s.mode = mode
    servers.append(s)
print(servers[0].server_port, servers[1].server_port, servers[2].server_port, flush=True)
for s in servers[1:]:
    threading.Thread(target=s.serve_forever, daemon=True).start()
servers[0].serve_forever()
PY
  SRV_PID=$!
  trap 'kill "$SRV_PID" 2>/dev/null || true; wait "$SRV_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
  for _ in {1..50}; do [[ -s "$WORK/port" ]] && break; sleep 0.1; done
  read -r OK_PORT UNREADY_PORT REDIRECT_PORT < "$WORK/port"

  out=$(HEALTH_INTERVAL_SECONDS=0 HEALTH_MAX_ATTEMPTS=3 HEALTH_REQUIRED_STREAK=3 \
    "$SUT" "http://127.0.0.1:${OK_PORT}" 2>&1) && real_ok=0 || real_ok=$?
  if [[ "$real_ok" -eq 0 && "$out" == *"Service is ready."* ]]; then
    echo "PASS: real curl accepts a 200 /ready"
    ((pass++)) || true
  else
    echo "FAIL: real curl against a 200 /ready (exit $real_ok)"; echo "$out"
    ((fail++)) || true
  fi

  out=$(HEALTH_INTERVAL_SECONDS=0 HEALTH_MAX_ATTEMPTS=2 HEALTH_REQUIRED_STREAK=2 \
    "$SUT" "http://127.0.0.1:${UNREADY_PORT}" 2>&1) && real_503=0 || real_503=$?
  if [[ "$real_503" -eq 1 && "$out" == *'"status":"degraded"'* ]]; then
    echo "PASS: real curl rejects a 503 /ready and prints its body"
    ((pass++)) || true
  else
    echo "FAIL: real curl against a 503 /ready (exit $real_503)"; echo "$out"
    ((fail++)) || true
  fi

  # The reviewer's repro on #1025: a server answering 301 on every path must
  # NOT pass, and the failure must name where it redirected to.
  out=$(HEALTH_INTERVAL_SECONDS=0 HEALTH_MAX_ATTEMPTS=3 HEALTH_REQUIRED_STREAK=3 \
    "$SUT" "http://127.0.0.1:${REDIRECT_PORT}" 2>&1) && real_301=0 || real_301=$?
  if [[ "$real_301" -eq 1 && "$out" == *"HTTP 301 redirect to https://app.example.invalid/ready"* ]]; then
    echo "PASS: real curl rejects a 301 /ready and names the redirect target"
    ((pass++)) || true
  else
    echo "FAIL: real curl against a 301 /ready (exit $real_301)"; echo "$out"
    ((fail++)) || true
  fi
fi

echo "passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
