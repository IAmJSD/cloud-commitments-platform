#!/usr/bin/env bash
# test-migration-down-gate.sh
#
# Asserts that database-migration.yml cannot turn a `direction=down` request
# into `migrate down -all` (#68), at three levels:
#
#   1. check-migration-down-request.sh, the gate itself, over a table of real
#      and adversarial inputs against a fixture migrations directory.
#   2. The workflow's own `validate` "Safety checks" block, extracted from the
#      YAML and executed, so the gate is proven wired in rather than merely
#      present on disk.
#   3. Each migrate-* job's "Run migrations" block, extracted and executed with
#      a stub `migrate` on PATH, so the defense-in-depth copy is proven to stop
#      a request before migrate is ever invoked.
#
# golang-migrate's readDown() treats `down N` with N at or past the applied
# count as "roll back everything that remains" and only returns ErrShortLimit
# afterwards, so steps=99999999 plus the old bare "rollback-prod" confirmation
# dropped the whole prod schema. Every case below that names that input is the
# reproduction from the issue.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GATE="${SCRIPT_DIR}/check-migration-down-request.sh"
WORKFLOW="${REPO_ROOT}/.github/workflows/database-migration.yml"
REAL_MIGRATIONS="${REPO_ROOT}/internal/database/postgres/migrations"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

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

# --- Fixtures ---------------------------------------------------------------

# Five versions: 1-4 with both files, 5 with an up file only (golang-migrate
# still counts it as a version), plus files migrate ignores.
FIXTURE="${WORK}/migrations"
mkdir -p "$FIXTURE"
for v in 000001 000002 000003 000004; do
  : >"${FIXTURE}/${v}_m${v}.up.sql"
  : >"${FIXTURE}/${v}_m${v}.down.sql"
done
: >"${FIXTURE}/000005_m5.up.sql"
: >"${FIXTURE}/000005_m5_test.go"
: >"${FIXTURE}/migrate.go"
: >"${FIXTURE}/README.down.sql"
FIXTURE_TOTAL=5

EMPTY_DIR="${WORK}/empty"
mkdir -p "$EMPTY_DIR"

# A stub `migrate` that records its arguments and succeeds.
STUB_BIN="${WORK}/bin"
STUB_LOG="${WORK}/migrate.log"
mkdir -p "$STUB_BIN"
cat >"${STUB_BIN}/migrate" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"${STUB_LOG}"
EOF
chmod +x "${STUB_BIN}/migrate"

# --- 1. The gate script ------------------------------------------------------

# gate_case LABEL EXPECTED_EXIT ENVIRONMENT STEPS CONFIRM DIR
gate_case() {
  local label="$1" expected="$2"
  shift 2
  local rc=0
  bash "$GATE" "$@" >"${WORK}/gate.out" 2>&1 || rc=$?
  if [[ "$rc" -eq "$expected" ]]; then
    note_pass "gate: $label"
  else
    note_fail "gate: $label (expected exit $expected, got $rc)"
    sed 's/^/      /' "${WORK}/gate.out"
  fi
}

# Positive cases first: a gate that rejects everything passes every negative.
gate_case "prod steps=1 with rollback-prod-1" 0 prod 1 rollback-prod-1 "$FIXTURE"
gate_case "prod steps=TOTAL-1 with matching confirmation" 0 prod 4 rollback-prod-4 "$FIXTURE"
gate_case "dev steps=1 needs no confirmation" 0 dev 1 "" "$FIXTURE"
gate_case "staging steps=TOTAL-1 needs no confirmation" 0 staging 4 "" "$FIXTURE"

# The issue's reproduction, with old and new confirmation strings.
gate_case "prod steps=99999999 with bare rollback-prod (#68 repro)" 1 prod 99999999 rollback-prod "$FIXTURE"
gate_case "prod steps=99999999 with rollback-prod-99999999" 1 prod 99999999 rollback-prod-99999999 "$FIXTURE"
gate_case "prod steps=TOTAL with matching confirmation" 1 prod 5 rollback-prod-5 "$FIXTURE"
gate_case "prod steps=TOTAL+1 with matching confirmation" 1 prod 6 rollback-prod-6 "$FIXTURE"

# The cap holds in every environment.
gate_case "dev steps=TOTAL" 1 dev 5 "" "$FIXTURE"
gate_case "staging steps=99999999" 1 staging 99999999 "" "$FIXTURE"

# Values that wrap in bash arithmetic: 2^64+1 evaluates to 1, 2^64+4 to 4.
gate_case "dev steps=2^64+1 (wraps to 1)" 1 dev 18446744073709551617 "" "$FIXTURE"
gate_case "prod steps=2^64+4 (wraps to 4) with matching confirmation" 1 prod 18446744073709551620 rollback-prod-18446744073709551620 "$FIXTURE"

# The confirmation encodes the magnitude, exactly.
gate_case "prod steps=1 with bare rollback-prod" 1 prod 1 rollback-prod "$FIXTURE"
gate_case "prod steps=1 with empty confirmation" 1 prod 1 "" "$FIXTURE"
gate_case "prod steps=2 with rollback-prod-3" 1 prod 2 rollback-prod-3 "$FIXTURE"
gate_case "prod steps=1 with rollback-prod-01" 1 prod 1 rollback-prod-01 "$FIXTURE"
gate_case "prod steps=1 with trailing space" 1 prod 1 "rollback-prod-1 " "$FIXTURE"
gate_case "prod steps=1 with ROLLBACK-PROD-1" 1 prod 1 ROLLBACK-PROD-1 "$FIXTURE"

# Malformed step counts.
for bad in "0" "" "-1" "01" "1e3" " 1" "1 " "1.0" "+1" "0x1"; do
  gate_case "dev steps='${bad}'" 1 dev "$bad" "" "$FIXTURE"
done
gate_case "prod steps=0 with rollback-prod-0" 1 prod 0 rollback-prod-0 "$FIXTURE"

# No versions to bound the rollback against.
gate_case "empty migrations directory" 1 dev 1 "" "$EMPTY_DIR"
gate_case "missing migrations directory" 1 dev 1 "" "${WORK}/does-not-exist"

# Wrong arity is a usage error, not a pass.
gate_case "missing arguments" 2 prod 1

# Against the real tree: the bound is the number of versions it holds.
REAL_TOTAL=$(find "$REAL_MIGRATIONS" -maxdepth 1 -type f \( -name '[0-9]*_*.up.sql' -o -name '[0-9]*_*.down.sql' \) |
  sed 's#.*/##; s/_.*//' | sort -u | wc -l | tr -d ' ')
if [[ "$REAL_TOTAL" -lt 2 ]]; then
  note_fail "real tree: expected at least 2 migration versions, found $REAL_TOTAL"
else
  last_ok=$((REAL_TOTAL - 1))
  gate_case "real tree: prod steps=1" 0 prod 1 rollback-prod-1 "$REAL_MIGRATIONS"
  gate_case "real tree: prod steps=$last_ok (TOTAL-1)" 0 prod "$last_ok" "rollback-prod-$last_ok" "$REAL_MIGRATIONS"
  gate_case "real tree: prod steps=$REAL_TOTAL (TOTAL)" 1 prod "$REAL_TOTAL" "rollback-prod-$REAL_TOTAL" "$REAL_MIGRATIONS"
fi

# --- Workflow extraction -----------------------------------------------------

# extract_step JOB STEP_NAME prints the YAML lines of one step: from its
# `- name: STEP_NAME` line up to the next step or job.
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

# load_block VAR JOB STEP stores the run block in VAR, failing the test if it
# is missing or still holds an unrendered ${{ }} expression this harness cannot
# evaluate.
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

# --- 2. The validate job -----------------------------------------------------

# validate_case LABEL EXPECTED_IS_SAFE ENVIRONMENT DIRECTION STEPS CONFIRM DIR
validate_case() {
  local label="$1" expected="$2" env="$3" dir="$4" steps="$5" confirm="$6" mpath="$7"
  local out="${WORK}/github_output" got
  : >"$out"
  (
    cd "$REPO_ROOT"
    CLOUD=aws ENVIRONMENT="$env" DIRECTION="$dir" STEPS="$steps" CONFIRM="$confirm" \
      COMPUTE_PLATFORM=lambda MIGRATIONS_PATH="$mpath" GITHUB_OUTPUT="$out" \
      bash -c "$VALIDATE_BLOCK"
  ) >"${WORK}/validate.out" 2>&1 || true
  got="$(sed -n 's/^is_safe=//p' "$out")"
  if [[ "$got" == "$expected" ]]; then
    note_pass "validate: $label"
  else
    note_fail "validate: $label (expected is_safe=$expected, got '${got}')"
    sed 's/^/      /' "${WORK}/validate.out"
  fi
}

STEP_TEXT=""
VALIDATE_BLOCK=""
if load_block VALIDATE_BLOCK validate "Safety checks"; then
  validate_case "prod down steps=1 with rollback-prod-1" true prod down 1 rollback-prod-1 "$FIXTURE"
  validate_case "dev down steps=1" true dev down 1 "" "$FIXTURE"
  validate_case "prod up steps=0 is untouched" true prod up 0 "" "$FIXTURE"
  validate_case "prod up steps=99999999 is untouched" true prod up 99999999 "" "$FIXTURE"
  validate_case "prod down steps=99999999 with rollback-prod (#68 repro)" false prod down 99999999 rollback-prod "$FIXTURE"
  validate_case "prod down steps=99999999 with rollback-prod-99999999" false prod down 99999999 rollback-prod-99999999 "$FIXTURE"
  validate_case "prod down steps=1 with bare rollback-prod" false prod down 1 rollback-prod "$FIXTURE"
  validate_case "dev down steps=TOTAL" false dev down "$FIXTURE_TOTAL" "" "$FIXTURE"
  validate_case "dev down steps=0" false dev down 0 "" "$FIXTURE"
fi

# --- 3. The migrate-* jobs ---------------------------------------------------

# migrate_case JOB LABEL EXPECTED_ARGS ENVIRONMENT DIRECTION STEPS CONFIRM
# EXPECTED_ARGS is the trailing argument list the stub must have received for
# the migrate run, or "" when migrate must not run at all.
migrate_case() {
  local job="$1" label="$2" expected="$3" env="$4" dir="$5" steps="$6" confirm="$7"
  local got
  : >"$STUB_LOG"
  (
    cd "$REPO_ROOT"
    PATH="${STUB_BIN}:$PATH" PGPASSWORD=unused DB_ENDPOINT=db.invalid \
      ENVIRONMENT="$env" DIRECTION="$dir" STEPS="$steps" CONFIRM="$confirm" \
      MIGRATIONS_PATH="$FIXTURE" bash -c "$MIGRATE_BLOCK"
  ) >"${WORK}/migrate.out" 2>&1 || true
  got="$(sed -E 's/^-path [^ ]+ -database [^ ]+ ?//' "$STUB_LOG")"
  if [[ "$got" == "$expected" ]]; then
    note_pass "$job: $label"
  else
    note_fail "$job: $label (expected migrate args '${expected}', got '${got}')"
    sed 's/^/      /' "${WORK}/migrate.out"
  fi
}

for job in migrate-aws migrate-gcp migrate-azure; do
  MIGRATE_BLOCK=""
  load_block MIGRATE_BLOCK "$job" "Run migrations" || continue

  # The gate reads ENVIRONMENT and CONFIRM, so the step must map them in.
  for var in "ENVIRONMENT: \${{ inputs.environment }}" "CONFIRM: \${{ inputs.confirm }}"; do
    if grep -qF -- "$var" <<<"$STEP_TEXT"; then
      note_pass "$job: Run migrations maps '$var'"
    else
      note_fail "$job: Run migrations does not map '$var' into env"
    fi
  done

  migrate_case "$job" "prod down steps=1 with rollback-prod-1 runs" "down 1" prod down 1 rollback-prod-1
  migrate_case "$job" "dev down steps=TOTAL-1 runs" "down 4" dev down 4 ""
  migrate_case "$job" "prod up steps=0 runs" "up" prod up 0 ""
  migrate_case "$job" "prod up steps=2 runs" "up 2" prod up 2 ""
  migrate_case "$job" "prod down steps=99999999 with rollback-prod never reaches migrate (#68 repro)" "" prod down 99999999 rollback-prod
  migrate_case "$job" "prod down steps=99999999 with rollback-prod-99999999 never reaches migrate" "" prod down 99999999 rollback-prod-99999999
  migrate_case "$job" "prod down steps=1 with bare rollback-prod never reaches migrate" "" prod down 1 rollback-prod
  migrate_case "$job" "dev down steps=TOTAL never reaches migrate" "" dev down "$FIXTURE_TOTAL" ""
  migrate_case "$job" "dev down steps=0 never reaches migrate" "" dev down 0 ""
done

echo ""
echo "Results: ${pass} passed, ${fail} failed"
if [[ "$fail" -ne 0 ]]; then
  exit 1
fi
