#!/usr/bin/env bash
# test-read-tf-outputs.sh
#
# Offline test for read-tf-outputs.sh and its wiring into deploy-azure.yml
# (#438). `terraform` and `az` are stubs on PATH; nothing reaches a real
# backend or Azure.
#
#   1. read-tf-outputs.sh itself: clean values, setup-terraform wrapper noise,
#      a failing `terraform output`, empty and multi-line values, usage errors,
#      and that a failure leaves $GITHUB_OUTPUT untouched.
#   2. deploy-azure.yml's "Terraform Apply" run block, extracted and executed,
#      so a missing output is proven to fail the step that reads it (the old
#      `2>/dev/null ... || echo ""` form passed with an empty value).
#   3. deploy-azure.yml's "Check Container App logs" step: it runs after a
#      failed step (`always()`), skips with a warning when there is no app to
#      read from, and still fails on an az lookup error.
#
# WORKFLOW may point at another copy of deploy-azure.yml, e.g. the pre-fix one,
# to show the workflow cases fail there.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SUT="${SCRIPT_DIR}/read-tf-outputs.sh"
WORKFLOW="${WORKFLOW:-${REPO_ROOT}/.github/workflows/deploy-azure.yml}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin" "$WORK/tf"

pass=0
fail=0

note_pass() {
  echo "PASS: $1"
  ((pass++)) || true
}

note_fail() {
  echo "FAIL: $1"
  ((fail++)) || true
}

indent() {
  local line
  while IFS= read -r line; do printf '      %s\n' "$line"; done <<<"$1"
}

# Stub terraform. `terraform output -raw NAME` prints $TF_STUB/NAME.out
# verbatim (no newline added, as -raw does), NAME.err to stderr, and exits with
# NAME.rc (default 0). A NAME with no .out file behaves like a missing output.
# Every call is logged to $TF_STUB/calls. `terraform apply` succeeds.
cat > "$WORK/bin/terraform" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$TF_STUB/calls"
case "$1" in
  apply) echo "Apply complete!"; exit 0 ;;
  output) ;;
  *) echo "stub terraform: unexpected command $1" >&2; exit 64 ;;
esac
[ "$2" = "-raw" ] || { echo "stub terraform: expected -raw" >&2; exit 64; }
name="$3"
if [ ! -f "$TF_STUB/$name.out" ]; then
  printf '\nError: Output "%s" not found\n' "$name" >&2
  exit 1
fi
cat "$TF_STUB/$name.out"
[ -f "$TF_STUB/$name.err" ] && cat "$TF_STUB/$name.err" >&2
exit "$(cat "$TF_STUB/$name.rc" 2>/dev/null || echo 0)"
STUB

# Stub az. Logs its arguments to $AZ_LOG and exits with $AZ_RC (default 0).
cat > "$WORK/bin/az" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$AZ_LOG"
if [ "${AZ_RC:-0}" != 0 ]; then
  echo "ERROR: (ResourceGroupNotFound) Resource group 'rg-x' could not be found." >&2
  exit "$AZ_RC"
fi
echo "log line"
STUB
chmod +x "$WORK/bin/terraform" "$WORK/bin/az"

# reset_tf clears the stub's outputs and call log.
reset_tf() {
  rm -rf "$WORK/tf"
  mkdir -p "$WORK/tf"
  : > "$WORK/tf/calls"
}

# set_output NAME VALUE [RC]: VALUE is written with printf %b, no newline.
set_output() {
  printf '%b' "$2" > "$WORK/tf/$1.out"
  [ -z "${3:-}" ] || echo "$3" > "$WORK/tf/$1.rc"
}

# run_sut ARGS...: runs the script, setting RC, OUT (stdout+stderr) and GHO
# (the resulting $GITHUB_OUTPUT contents, which starts with one sentinel line).
run_sut() {
  printf 'existing=1\n' > "$WORK/gho"
  RC=0
  OUT=$(PATH="$WORK/bin:$PATH" TF_STUB="$WORK/tf" GITHUB_OUTPUT="$WORK/gho" \
    bash "$SUT" "$@" 2>&1) || RC=$?
  GHO="$(cat "$WORK/gho")"
}

# expect LABEL EXPECTED_RC EXPECTED_GHO [OUTPUT_SUBSTRING]
expect() {
  local label="$1" want_rc="$2" want_gho="$3" needle="${4:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    note_fail "$label (expected exit $want_rc, got $RC)"
    indent "$OUT"
  elif [[ "$GHO" != "$want_gho" ]]; then
    note_fail "$label (GITHUB_OUTPUT mismatch)"
    printf '      want: %q\n      got:  %q\n' "$want_gho" "$GHO"
  elif [[ -n "$needle" && "$OUT" != *"$needle"* ]]; then
    note_fail "$label (output lacks '$needle')"
    indent "$OUT"
  else
    note_pass "$label"
  fi
}

# --- 1. read-tf-outputs.sh ---------------------------------------------------

reset_tf
set_output container_app_url 'https://app.example.azurecontainerapps.io'
set_output container_app_name 'cudly-dev-app'
set_output resource_group_name 'cudly-dev-rg'
run_sut app_url=container_app_url app_name=container_app_name resource_group_name=resource_group_name
expect "three clean outputs are written in order" 0 \
  $'existing=1\napp_url=https://app.example.azurecontainerapps.io\napp_name=cudly-dev-app\nresource_group_name=cudly-dev-rg'
if [[ "$(cat "$WORK/tf/calls")" == $'output -raw container_app_url\noutput -raw container_app_name\noutput -raw resource_group_name' ]]; then
  note_pass "terraform is called as 'output -raw NAME' for each output"
else
  note_fail "terraform calls were: $(cat "$WORK/tf/calls")"
fi

# What the setup-terraform wrapper prints around the value.
reset_tf
set_output container_app_url 'https://app.example.azurecontainerapps.io\n::debug::Terraform exited with code 0.\n::debug::stdout: https://app.example.azurecontainerapps.io\n::debug::stderr: \n::debug::exitcode: 0\n'
run_sut app_url=container_app_url
expect "setup-terraform wrapper ::debug:: lines are dropped" 0 \
  $'existing=1\napp_url=https://app.example.azurecontainerapps.io'

reset_tf
set_output container_app_url 'https://app.example.azurecontainerapps.io\n'
run_sut app_url=container_app_url
expect "a trailing newline is not part of the value" 0 \
  $'existing=1\napp_url=https://app.example.azurecontainerapps.io'

# The #438 case: the output does not exist. terraform's error must reach the
# log, the script must fail, and nothing must be written, including the
# outputs read successfully before the failing one.
reset_tf
set_output container_app_url 'https://app.example.azurecontainerapps.io'
run_sut app_url=container_app_url app_name=container_app_name
expect "missing output fails with terraform's error" 1 'existing=1' 'Error: Output "container_app_name" not found'
expect "missing output names the failing output" 1 'existing=1' '::error::terraform output -raw container_app_name failed'

reset_tf
set_output container_app_url '' 1
printf 'Error: Failed to load state: blob not found\n' > "$WORK/tf/container_app_url.err"
run_sut app_url=container_app_url
expect "state read error fails and keeps the stderr" 1 'existing=1' 'Failed to load state'

reset_tf
set_output container_app_url ''
run_sut app_url=container_app_url
expect "empty value fails" 1 'existing=1' 'terraform output container_app_url is empty'

reset_tf
set_output container_app_url '\n  \n'
run_sut app_url=container_app_url
expect "whitespace-only value fails" 1 'existing=1' 'is empty'

reset_tf
set_output container_app_url '::debug::Terraform exited with code 0.\n::debug::exitcode: 0\n'
run_sut app_url=container_app_url
expect "wrapper noise with no value fails and shows the raw stdout" 1 'existing=1' '| ::debug::exitcode: 0'

reset_tf
set_output container_app_url 'Warning: something\nhttps://app.example.azurecontainerapps.io'
run_sut app_url=container_app_url
expect "two value lines fail instead of guessing" 1 'existing=1' 'has 2 lines, expected one'

reset_tf
run_sut
expect "no arguments is a usage error" 2 'existing=1' 'usage:'
run_sut app_url
expect "argument without = is a usage error" 2 'existing=1' "malformed argument 'app_url'"
run_sut 'app url=container_app_url'
expect "key with a space is a usage error" 2 'existing=1' 'malformed argument'
run_sut 'app_url=container_app_url;id'
expect "output name with shell metacharacters is a usage error" 2 'existing=1' 'malformed argument'
run_sut app_url=
expect "empty output name is a usage error" 2 'existing=1' 'malformed argument'
if [[ ! -s "$WORK/tf/calls" ]]; then
  note_pass "usage errors never call terraform"
else
  note_fail "usage errors called terraform: $(cat "$WORK/tf/calls")"
fi

RC=0
OUT=$(env -u GITHUB_OUTPUT PATH="$WORK/bin:$PATH" TF_STUB="$WORK/tf" \
  bash "$SUT" app_url=container_app_url 2>&1) || RC=$?
GHO='existing=1'
expect "unset GITHUB_OUTPUT is a usage error" 2 'existing=1' 'GITHUB_OUTPUT is not set'

# --- Workflow extraction -----------------------------------------------------

# extract_step JOB STEP_NAME prints the YAML lines of one step: from its
# `- name: STEP_NAME` line up to the next step or job. Same helper as
# test-migration-down-gate.sh.
extract_step() {
  awk -v job="  $1:" -v step="- name: $2" '
    $0 == job { injob = 1; next }
    injob && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit }
    injob && !instep {
      line = $0; sub(/^[[:space:]]+/, "", line)
      if (line == step) { instep = 1; match($0, /^ */); ind = RLENGTH; print; next }
    }
    instep {
      if ($0 ~ /^[[:space:]]*-[[:space:]]/) { match($0, /^ */); if (RLENGTH <= ind) exit }
      print
    }
  ' "$WORKFLOW"
}

# run_block prints the dedented body of the `run: |` block in a step's text.
run_block() {
  awk '
    !inrun && /^[[:space:]]+run: \|[[:space:]]*$/ { inrun = 1; match($0, /^ */); ind = RLENGTH; next }
    inrun {
      if ($0 ~ /^[[:space:]]*$/) { print ""; next }
      match($0, /^ */)
      if (RLENGTH <= ind) exit
      print substr($0, ind + 3)
    }
  '
}

# load_block VAR JOB STEP stores the run block in VAR and the step text in
# STEP_TEXT, failing the test if the block is missing or holds a ${{ }}
# expression this harness cannot evaluate.
load_block() {
  local step out
  step="$(extract_step "$2" "$3")"
  out="$(run_block <<<"$step")"
  if [[ -z "$out" ]]; then
    note_fail "workflow: no run block found for $2 / $3"
    return 1
  fi
  if [[ "$out" == *"\${{"* ]]; then
    note_fail "workflow: $2 / $3 run block holds a \${{ }} expression; move it to env:"
    return 1
  fi
  STEP_TEXT="$step"
  printf -v "$1" '%s' "$out"
}

# --- 2. Terraform Apply step -------------------------------------------------

# run_apply_block: runs the extracted block the way the runner does (bash -e,
# from the workspace root), against a scratch workspace whose scripts/ is the
# real one. Sets RC, OUT and GHO like run_sut.
run_apply_block() {
  local ws="$WORK/ws"
  rm -rf "$ws"
  mkdir -p "$ws/terraform/environments/azure" "$WORK/runner_temp"
  ln -s "$REPO_ROOT/scripts" "$ws/scripts"
  printf 'existing=1\n' > "$WORK/gho"
  RC=0
  OUT=$(cd "$ws" && PATH="$WORK/bin:$PATH" TF_STUB="$WORK/tf" GITHUB_OUTPUT="$WORK/gho" \
    GITHUB_WORKSPACE="$ws" RUNNER_TEMP="$WORK/runner_temp" \
    bash -e -c "$APPLY_BLOCK" 2>&1) || RC=$?
  GHO="$(cat "$WORK/gho")"
}

STEP_TEXT=""
APPLY_BLOCK=""
if load_block APPLY_BLOCK build-and-deploy "Terraform Apply"; then
  reset_tf
  set_output container_app_url 'https://app.example.azurecontainerapps.io\n::debug::exitcode: 0\n'
  set_output container_app_name 'cudly-dev-app'
  set_output resource_group_name 'cudly-dev-rg'
  run_apply_block
  expect "workflow apply: outputs reach GITHUB_OUTPUT" 0 \
    $'existing=1\napp_url=https://app.example.azurecontainerapps.io\napp_name=cudly-dev-app\nresource_group_name=cudly-dev-rg'

  # Pre-fix this exited 0 and wrote `resource_group_name=` (empty), so the
  # failure surfaced later in test-deployment as "Terraform outputs missing".
  reset_tf
  set_output container_app_url 'https://app.example.azurecontainerapps.io'
  set_output container_app_name 'cudly-dev-app'
  run_apply_block
  if [[ "$RC" -ne 0 && "$OUT" == *'Error: Output "resource_group_name" not found'* ]]; then
    note_pass "workflow apply: a missing output fails the step with terraform's error"
  else
    note_fail "workflow apply: a missing output did not fail the step loudly (exit $RC)"
    indent "$OUT"
  fi
  if [[ "$GHO" != *"resource_group_name="* ]]; then
    note_pass "workflow apply: no empty resource_group_name is published"
  else
    note_fail "workflow apply: published '$(grep resource_group_name= <<<"$GHO")'"
  fi
fi

if grep -nE 'terraform output.*(2>/dev/null|\|\| *echo)' "$WORKFLOW" > "$WORK/swallow"; then
  note_fail "workflow: a terraform output error is still swallowed:"
  sed 's/^/      /' "$WORK/swallow"
else
  note_pass "workflow: no terraform output call discards its error"
fi

# --- 3. Check Container App logs step ----------------------------------------

# run_logs_block APP_NAME RESOURCE_GROUP_NAME [AZ_RC]: sets RC, OUT and AZ.
run_logs_block() {
  : > "$WORK/az.log"
  RC=0
  OUT=$(PATH="$WORK/bin:$PATH" AZ_LOG="$WORK/az.log" AZ_RC="${3:-0}" \
    APP_NAME="$1" RESOURCE_GROUP_NAME="$2" bash -e -c "$LOGS_BLOCK" 2>&1) || RC=$?
  AZ="$(cat "$WORK/az.log")"
}

LOGS_BLOCK=""
if load_block LOGS_BLOCK test-deployment "Check Container App logs"; then
  if_line="$(grep -E '^[[:space:]]+if:' <<<"$STEP_TEXT" || true)"
  if [[ "$if_line" == *"always()"* ]]; then
    note_pass "workflow logs: step runs after a failed health check (always())"
  else
    note_fail "workflow logs: step has no always() condition, so it is skipped exactly when the health check fails (if: '${if_line}')"
  fi

  # The condition may reference the login step's outcome; that id must exist.
  if [[ "$if_line" == *"steps.azure-login."* ]]; then
    if extract_step test-deployment "Azure Login" | grep -qE '^[[:space:]]+id: azure-login[[:space:]]*$'; then
      note_pass "workflow logs: steps.azure-login refers to the Azure Login step"
    else
      note_fail "workflow logs: condition uses steps.azure-login but the Azure Login step has no 'id: azure-login'"
    fi
  fi

  run_logs_block cudly-dev-app cudly-dev-rg
  if [[ "$RC" -eq 0 && "$AZ" == "containerapp logs show --name cudly-dev-app --resource-group cudly-dev-rg --tail 50" ]]; then
    note_pass "workflow logs: reads the app's logs"
  else
    note_fail "workflow logs: happy path (exit $RC, az: '$AZ')"
    indent "$OUT"
  fi

  run_logs_block cudly-dev-app cudly-dev-rg 3
  if [[ "$RC" -ne 0 && "$OUT" == *"ResourceGroupNotFound"* ]]; then
    note_pass "workflow logs: an az lookup error still fails the step (#217)"
  else
    note_fail "workflow logs: an az lookup error was swallowed (exit $RC)"
  fi

  run_logs_block "" ""
  if [[ "$RC" -eq 0 && -z "$AZ" && "$OUT" == *"::warning::"* ]]; then
    note_pass "workflow logs: no deployed app warns and skips az"
  else
    note_fail "workflow logs: missing outputs (exit $RC, az: '$AZ')"
    indent "$OUT"
  fi
fi

echo "passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
