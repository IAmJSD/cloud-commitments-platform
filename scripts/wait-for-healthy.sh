#!/usr/bin/env bash
# wait-for-healthy.sh URL
#
# Smoke gate for a just-deployed service. Polls URL/ready until it answers a
# 2xx status HEALTH_REQUIRED_STREAK times in a row. A not-ready or failed
# response inside the polling budget resets the streak instead of failing,
# because during a rollout a probe can hit a replica that is still
# initializing. If the budget (HEALTH_MAX_ATTEMPTS) runs out before the streak
# is reached, exits 1 and prints the last response (or curl error).
#
# /ready is the traffic-admission endpoint: it answers 503 until the replica it
# answers for has completed initialization, so the gate reflects the same
# contract the load balancer uses (#488). /health answers 200 even when
# degraded and is a liveness signal only. No warm-up request is needed: the
# server initializes its database in the background at startup.
#
# Only 2xx counts as ready. A 3xx is NOT ready and is logged with its redirect
# target: an http:// URL in front of an HTTPS-only listener answers 301 on every
# path, and counting that as ready would make the gate pass without ever
# reaching the app (#647). Redirects are deliberately not followed; point the
# gate at the URL that serves the app instead. A non-2xx body is kept so a 503
# still prints the JSON check detail that explains which dependency is not
# ready.
#
# Env (defaults suit the deploy workflows; tests shrink them):
#   HEALTH_REQUIRED_STREAK   consecutive ready responses needed (3)
#   HEALTH_MAX_ATTEMPTS      polling rounds before failing (30)
#   HEALTH_INTERVAL_SECONDS  sleep between rounds (5)

set -euo pipefail

URL="${1:?usage: wait-for-healthy.sh URL}"
REQUIRED_STREAK="${HEALTH_REQUIRED_STREAK-3}"
MAX_ATTEMPTS="${HEALTH_MAX_ATTEMPTS-30}"
INTERVAL="${HEALTH_INTERVAL_SECONDS:-5}"

# Fail loudly on bad limits: 0 or non-numeric would silently behave like 1, and
# attempts < streak can never pass. A set-but-empty value is rejected too.
for pair in "HEALTH_REQUIRED_STREAK=${REQUIRED_STREAK}" "HEALTH_MAX_ATTEMPTS=${MAX_ATTEMPTS}"; do
  if [[ ! "${pair#*=}" =~ ^[1-9][0-9]*$ ]]; then
    echo "::error::${pair%%=*} must be a positive integer, got '${pair#*=}'" >&2
    exit 2
  fi
done
if ((MAX_ATTEMPTS < REQUIRED_STREAK)); then
  echo "::error::HEALTH_MAX_ATTEMPTS (${MAX_ATTEMPTS}) is below HEALTH_REQUIRED_STREAK (${REQUIRED_STREAK}); the check could never pass" >&2
  exit 2
fi

streak=0
last="no response"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
  # curl exits 0 for any HTTP status here (no -f), so the status is judged
  # below. stderr goes to its own file: curl prints its -w output even when the
  # request fails, and mixing the two would garble the error message.
  : > "$tmp/body"
  if meta=$(curl -sS --max-time 10 -o "$tmp/body" -w '%{http_code} %{redirect_url}' "${URL}/ready" 2> "$tmp/err"); then
    code="${meta%% *}"
    redirect="${meta#* }"
    [[ "$redirect" == "$meta" ]] && redirect=""
    body="$(cat "$tmp/body")"
    case "$code" in
      2??)
        streak=$((streak + 1))
        echo "Readiness check ${attempt}: ready (${streak}/${REQUIRED_STREAK})"
        if ((streak >= REQUIRED_STREAK)); then
          echo "Service is ready."
          exit 0
        fi
        sleep "$INTERVAL"
        continue
        ;;
      3??)
        last="HTTP ${code} redirect to ${redirect:-<no Location header>}; redirects are not followed, so point the gate at the URL that serves the app"
        ;;
      *)
        last="HTTP ${code}: ${body:-<empty body>}"
        ;;
    esac
  else
    err="$(cat "$tmp/err")"
    last="${err:-no response}"
  fi
  streak=0
  echo "Readiness check ${attempt}: not ready yet, streak reset: ${last}"
  sleep "$INTERVAL"
done

echo "::error::Service not ready ${REQUIRED_STREAK}x in a row after ${MAX_ATTEMPTS} attempts. Last response: ${last}"
exit 1
