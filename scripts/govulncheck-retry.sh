#!/usr/bin/env bash
# govulncheck-retry.sh [govulncheck args...]
#
# Runs govulncheck, retrying only when the vulnerability database fetch failed
# transiently (vuln.go.dev dropping the connection, issue #74).
#
# Exit codes at the pinned v1.1.4 (internal/scan/errors.go, cmd/govulncheck/main.go):
# 0 clean, 3 vulnerabilities found, 2 usage error, 1 any other error. A real
# finding (3) is never retried. Exit 1 is retried only when stderr shows the DB
# fetch failing on a network error, so a genuine tool or config error fails on
# the first attempt. Only exit 1 is retried. Stderr is buffered in a file and
# replayed after each attempt. Stdout of a failed attempt is
# passed through unbuffered, so with -format json/sarif the output of several
# attempts can be concatenated (CI does not use -format). After MAX_ATTEMPTS the last exit
# code is returned.
#
# GOVULNCHECK_RETRY_SLEEP: seconds of backoff per attempt number (default 15).

set -euo pipefail

MAX_ATTEMPTS=3
BACKOFF="${GOVULNCHECK_RETRY_SLEEP:-15}"
TRANSIENT='connection reset by peer|connection refused|i/o timeout|TLS handshake timeout|unexpected EOF|no such host|temporary failure|(^|[^0-9])(500|502|503|504)([^0-9]|$)'

errfile="$(mktemp "${TMPDIR:-/tmp}/govulncheck-retry.XXXXXX")"
trap 'rm -f "$errfile"' EXIT

attempt=1
while :; do
  rc=0
  # stderr goes straight to a file and is replayed afterwards: a tee process
  # substitution is not reliably flushed before the greps below read the file.
  govulncheck "$@" 2>"$errfile" || rc=$?
  cat "$errfile" >&2
  if [[ "$rc" -eq 0 || "$rc" -eq 3 ]]; then
    exit "$rc"
  fi
  if [[ "$rc" -ne 1 ]] || ! grep -q 'fetching vulnerabilities' "$errfile" || ! grep -Eqi "$TRANSIENT" "$errfile"; then
    echo "govulncheck-retry: exit $rc is not a transient vulnerability-DB fetch failure; not retrying" >&2
    exit "$rc"
  fi
  if [[ "$attempt" -ge "$MAX_ATTEMPTS" ]]; then
    echo "govulncheck-retry: vulnerability-DB fetch still failing after $MAX_ATTEMPTS attempts (last exit $rc)" >&2
    exit "$rc"
  fi
  echo "govulncheck-retry: transient fetch failure (exit $rc), retry $((attempt + 1))/$MAX_ATTEMPTS in $((BACKOFF * attempt))s" >&2
  sleep "$((BACKOFF * attempt))"
  attempt=$((attempt + 1))
done
