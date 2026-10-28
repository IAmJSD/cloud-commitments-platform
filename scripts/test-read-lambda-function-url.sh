#!/usr/bin/env bash
# test-read-lambda-function-url.sh
#
# Offline test for read-lambda-function-url.sh (#324): terraform is stubbed via
# PATH, so nothing reads real state or reaches AWS. Each case sets what the
# stubbed `terraform output -raw lambda_function_url` prints and how it exits.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUT="${SCRIPT_DIR}/read-lambda-function-url.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin" "$WORK/tf"

# The stub checks it was asked for exactly the output the script needs, then
# prints $STUB_STDOUT and exits $STUB_EXIT.
cat > "$WORK/bin/terraform" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" != "-chdir=$EXPECT_DIR output -raw lambda_function_url" ]]; then
  echo "unexpected terraform invocation: $*" >&2
  exit 99
fi
printf '%b' "$STUB_STDOUT"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "$WORK/bin/terraform"

pass=0
fail=0

# run_case NAME STDOUT EXIT WANT_EXIT WANT_OUTPUT
run_case() {
  local name="$1" stdout="$2" exit_code="$3" want_exit="$4" want_out="$5"
  local out got_exit=0
  out=$(PATH="$WORK/bin:$PATH" EXPECT_DIR="$WORK/tf" STUB_STDOUT="$stdout" STUB_EXIT="$exit_code" \
    bash "$SUT" "$WORK/tf" 2>/dev/null) || got_exit=$?
  if [[ "$got_exit" == "$want_exit" && "$out" == "$want_out" ]]; then
    echo "PASS: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name (exit $got_exit, want $want_exit; output '$out', want '$want_out')"
    fail=$((fail + 1))
  fi
}

url="https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws"

run_case "strips the trailing slash the output carries" "$url/" 0 0 "$url"
run_case "accepts a URL without a trailing slash" "$url" 0 0 "$url"
run_case "drops setup-terraform wrapper debug lines" "::debug::stdout\n$url/\n::debug::exitcode: 0" 0 0 "$url"
# `terraform output -raw` exits 1 with nothing on stdout both when the output
# does not exist yet (first deploy) and when it is null (a Fargate stack).
run_case "missing or null output is empty, not an error" "" 1 0 ""
run_case "rejects a non-Function-URL value" "https://evil.example.com/" 0 1 ""
run_case "rejects plain http" "http://abc.lambda-url.us-east-1.on.aws/" 0 1 ""
run_case "rejects a lookalike host suffix" "https://abc.lambda-url.us-east-1.on.aws.evil.example/" 0 1 ""
run_case "rejects a path after the host" "$url/x" 0 1 ""
run_case "rejects two URLs on separate lines" "$url/\n$url/" 0 1 ""

usage_exit=0
bash "$SUT" >/dev/null 2>&1 || usage_exit=$?
if [[ "$usage_exit" == 2 ]]; then
  echo "PASS: no argument is a usage error"
  pass=$((pass + 1))
else
  echo "FAIL: no argument exited $usage_exit, want 2"
  fail=$((fail + 1))
fi

echo
echo "Results: $pass passed, $fail failed."
[[ "$fail" -eq 0 ]]
