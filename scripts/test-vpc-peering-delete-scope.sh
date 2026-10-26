#!/usr/bin/env bash
# test-vpc-peering-delete-scope.sh
#
# Asserts that the GCP staging destroy deletes the Service Networking peering
# its Terraform state owns, on the network that state names, and removes it
# from state only after that delete succeeded (#330).
#
# The step this guards used to run `terraform state rm` on the module-qualified
# peering address FIRST, then `gcloud services vpc-peerings delete` on the
# literal network `cudly-staging-vpc` with `2>/dev/null || echo ...`. A failed
# or misdirected delete therefore reported success and left a live peering no
# state tracked. It also asserts that a failed peering delete still lets
# `terraform destroy` run and still fails the job. Fourth sibling of
# test-ecr-delete-selection.sh, test-rds-deletion-protection-scope.sh and
# test-cloud-sql-delete-scope.sh.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKFLOW_DIR="${REPO_ROOT}/.github/workflows"

# code_of() / invokes() / build_swept_scripts, shared with the sibling suites.
# shellcheck source=scripts/lib/code-scan-awk.sh
. "${SCRIPT_DIR}/lib/code-scan-awk.sh"

DELETE_SCRIPT="${SCRIPT_DIR}/delete-owned-vpc-peering.sh"
DELETE_CMD_RE='gcloud[[:space:]]+services[[:space:]]+vpc-peerings[[:space:]]+delete'
PEERING_ADDR="module.networking.google_service_networking_connection.private_vpc_connection"
# Read by the awk programs through ENVIRON, so a step name never has to be
# spliced into awk source.
export STEP="Delete the Service Networking peering this state owns"
export SQL_STEP="Delete the Cloud SQL instance this state owns"

pass=0
fail=0

# check LABEL CONDITION_RESULT DETAIL -- CONDITION_RESULT is "0" for a pass.
check() {
  if [[ "$2" == "0" ]]; then
    echo "PASS: $1"
    ((pass++)) || true
  else
    echo "FAIL: $1"
    echo "      $3"
    ((fail++)) || true
  fi
}

# awk_check LABEL FILE DETAIL AWK_PROGRAM -- the program's exit status decides.
awk_check() {
  local label="$1" file="$2" detail="$3" program="$4" rc=0
  if [[ ! -f "$file" ]]; then
    check "$label" 1 "file not found at ${file}"
    return
  fi
  awk -v SQ="'" -v cmdre="$DELETE_CMD_RE" "${AWK_CODE_FUNCS}${program}" "$file" || rc=$?
  check "$label" "$rc" "$detail"
}

# --- Terraform: the output names the network the peering is attached to -----
#
# The script trusts `network_name` to be the peering's network. Break any link
# of that chain and every behavior case below stays green while the real
# delete targets the wrong network (or none).

# block_has HEADER_RE LINE_RE -> an awk program passing when exactly one line
# inside the block opened by HEADER_RE matches LINE_RE. `[{]`/`[}]` rather than
# bare braces, which start an interval expression in ERE under mawk (CI's awk).
block_has() {
  printf '%s' '
    code_of($0) ~ /^'"$1"'[[:space:]]*[{]/ { in_block = 1; next }
    in_block && code_of($0) ~ /^[}]/ { in_block = 0 }
    in_block && code_of($0) ~ /'"$2"'/ { hits++ }
    END { exit !(hits == 1) }'
}

awk_check "the gcp environment publishes network_name from the networking module" \
  "${REPO_ROOT}/terraform/environments/gcp/outputs.tf" "expected exactly one match" \
  "$(block_has 'output[[:space:]]+"network_name"' 'value[[:space:]]*=[[:space:]]*module[.]networking[.]network_name[[:space:]]*$')"

awk_check "the networking module publishes the real google_compute_network name" \
  "${REPO_ROOT}/terraform/modules/networking/gcp/outputs.tf" "expected exactly one match" \
  "$(block_has 'output[[:space:]]+"network_name"' 'value[[:space:]]*=[[:space:]]*google_compute_network[.]main[.]name[[:space:]]*$')"

awk_check "the peering resource is attached to that same network" \
  "${REPO_ROOT}/terraform/modules/networking/gcp/main.tf" "expected exactly one match" \
  "$(block_has 'resource[[:space:]]+"google_service_networking_connection"[[:space:]]+"private_vpc_connection"' 'network[[:space:]]*=[[:space:]]*google_compute_network[.]main[.]id[[:space:]]*$')"

awk_check "the gcp environment instantiates that module as module.networking" \
  "${REPO_ROOT}/terraform/environments/gcp/networking.tf" "expected exactly one match" \
  "$(block_has 'module[[:space:]]+"networking"' 'source[[:space:]]*=[[:space:]]*"[.][.]\/[.][.]\/modules\/networking\/gcp"')"

# --- Wiring: the destroy job routes through the script, in order ------------
#
# Exactly one step with this name, running the script with the pinned state
# directory and project argument. The regex ends at end-of-line, so a trailing
# `|| true` fails it.
CLEANUP="${WORKFLOW_DIR}/cleanup-staging.yml"
awk_check "exactly one '${STEP}' step, and it calls the script" "$CLEANUP" \
  "expected one step running './scripts/delete-owned-vpc-peering.sh terraform/environments/gcp \"\$PROJECT\"' (step renamed, duplicated, call changed, or '|| true' appended)" '
    function finish() { if (in_step) { steps++; if (!has_call) unwired++ } in_step = 0; has_call = 0 }
    $0 ~ ("^[[:space:]]*-[[:space:]]+name:[[:space:]]*" ENVIRON["STEP"] "[[:space:]]*$") { finish(); in_step = 1; next }
    /^[[:space:]]*-[[:space:]]+name:/ { finish() }
    in_step && code_of($0) ~ /^[[:space:]]*[.]\/scripts\/delete-owned-vpc-peering[.]sh[[:space:]]+terraform\/environments\/gcp[[:space:]]+"[$]PROJECT"[[:space:]]*$/ { has_call = 1 }
    END { finish(); exit !(steps == 1 && unwired == 0) }'

# Inside the destroy-gcp job, the peering step sits after the Cloud SQL step
# (the peering cannot go while the instance uses it) and before the destroy
# (which fails on the network while the peering exists). Job keys are the
# two-space-indented `name:` lines under `jobs:`.
awk_check "destroy-gcp runs Cloud SQL delete, then the peering delete, then terraform destroy" "$CLEANUP" \
  "expected the three steps in that order inside the destroy-gcp job" '
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { in_job = ($0 ~ /^  destroy-gcp:/) }
    in_job && /^[[:space:]]*-[[:space:]]+name:/ {
      name = $0; sub(/^[[:space:]]*-[[:space:]]+name:[[:space:]]*/, "", name); sub(/[[:space:]]*$/, "", name)
      n++
      if (name == ENVIRON["SQL_STEP"]) sql = n
      if (name == ENVIRON["STEP"]) peer = n
      if (name == "Terraform Destroy") destroy = n
    }
    END { exit !(sql > 0 && peer == sql + 1 && destroy == peer + 1) }'

# --- Wiring: a failed peering delete still destroys, and still fails the job --
#
# Before the peering step existed, a failed peering delete only cost the
# network: `terraform destroy` had already removed everything else. A plain
# failing step would skip the destroy and leave the whole stack up (and
# billing). So the peering step is continue-on-error, and a later step fails
# the job on its outcome, so the failure is not swallowed. The script itself
# leaves the address in state on a failed delete (the behavior cases below).
#
# simulate_destroy_gcp FILE FAILING_STEP_NAMES -> one "ran NAME" / "skipped
# NAME" per destroy-gcp step, then "job success|failure". A small model of the
# runner's step semantics, for the subset of `if:` this job may use:
# success(), failure(), always(), cancelled(), !cancelled() and
# steps.ID.outcome|conclusion ==/!= 'VALUE', joined by `&&`. An `if:` with no
# status function gets the runner's implicit success(). A step fails when it
# is named in FAILING_STEP_NAMES (`|`-separated) or when the last code line of
# its run block is `exit 1`. continue-on-error makes a failed step's
# conclusion success while its outcome stays failure. Anything outside that
# subset (an `||`, a multi-line `if:`, an unknown function) is an error, so
# the model cannot silently misread a changed condition.
simulate_destroy_gcp() {
  FAILING="$2" awk -v SQ="'" "${AWK_CODE_FUNCS}"'
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function unsupported(what) { print "unsupported: " what; bad = 1; exit 2 }
    function value_of(line) { sub(/^[[:space:]]*(-[[:space:]]+)?[A-Za-z_-]+:[[:space:]]*/, "", line); return trim(code_of(line)) }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { in_job = ($0 ~ /^  destroy-gcp:/); in_run = 0; next }
    !in_job { next }
    /^      -[[:space:]]/ { n++; in_run = 0 }
    n == 0 { next }
    in_run && /^[[:space:]]*$/ { next }
    in_run && /^          / { c = trim(code_of($0)); if (c != "") last[n] = c; next }
    { in_run = 0 }
    /^      -[[:space:]]+name:/ || /^        name:/ { name[n] = value_of($0) }
    /^        id:/ { id[n] = value_of($0) }
    /^        if:/ { cond[n] = value_of($0); if (cond[n] ~ /^[|>]/) unsupported("multi-line if: in step " n) }
    /^        continue-on-error:/ {
      v = value_of($0)
      if (v != "true" && v != "false") unsupported("continue-on-error: " v)
      coe[n] = (v == "true")
    }
    /^        run:[[:space:]]*[|]/ { in_run = 1; next }
    /^        run:/ { last[n] = value_of($0) }
    END {
      if (bad) exit 2
      nf = split(ENVIRON["FAILING"], f, "|")
      for (i = 1; i <= nf; i++) failing[f[i]] = 1
      job_ok = 1
      for (i = 1; i <= n; i++) {
        e = cond[i]
        sub(/^[$][{][{]/, "", e); sub(/[}][}]$/, "", e); e = trim(e)
        run = 1; has_status = 0
        if (e != "") {
          nt = split(e, t, /&&/)
          for (k = 1; k <= nt; k++) {
            term = trim(t[k])
            if (term == "success()") { has_status = 1; r = job_ok }
            else if (term == "failure()") { has_status = 1; r = !job_ok }
            else if (term == "always()") { has_status = 1; r = 1 }
            else if (term == "!cancelled()") { has_status = 1; r = 1 }
            else if (term == "cancelled()") { has_status = 1; r = 0 }
            else if (term ~ ("^steps[.][A-Za-z0-9_-]+[.](outcome|conclusion)[[:space:]]*[!=]=[[:space:]]*" SQ "(success|failure|skipped|cancelled)" SQ "$")) {
              split(term, ref, /[.]/); sid = ref[2]
              field = (term ~ /[.]outcome/) ? "outcome" : "conclusion"
              want = term; sub("^[^" SQ "]*" SQ, "", want); sub(SQ "$", "", want)
              got = (field == "outcome") ? out_by_id[sid] : concl_by_id[sid]
              r = (term ~ /!=/) ? (got != want) : (got == want)
            }
            else unsupported("if term: " term)
            run = run && r
          }
        }
        if (!has_status) run = run && job_ok
        if (run) {
          fails = (name[i] in failing) || (last[i] ~ /^exit[[:space:]]+1$/)
          o = fails ? "failure" : "success"
          cl = (fails && !coe[i]) ? "failure" : "success"
          if (cl == "failure") job_ok = 0
          print "ran " name[i]
        } else {
          o = "skipped"; cl = "skipped"
          print "skipped " name[i]
        }
        if (id[i] != "") { out_by_id[id[i]] = o; concl_by_id[id[i]] = cl }
      }
      print "job " (job_ok ? "success" : "failure")
    }' "$1"
}

# destroy_gcp_failure_handling_rc FILE -> 0 when every scenario below holds.
destroy_gcp_failure_handling_rc() {
  local file="$1" out
  # Nothing fails: the job passes and destroy runs.
  out="$(simulate_destroy_gcp "$file" "")" || return 1
  [[ "$out" == *$'\nran Terraform Destroy\n'* && "$out" == *$'\njob success' ]] || return 1
  # The peering delete fails: destroy still runs, and the job fails.
  out="$(simulate_destroy_gcp "$file" "$STEP")" || return 1
  [[ "$out" == *$'\nran Terraform Destroy\n'* && "$out" == *$'\njob failure' ]] || return 1
  # Both fail: the job still fails (the reporting step is not skipped by the
  # destroy failure, so the peering failure stays named in the log).
  out="$(simulate_destroy_gcp "$file" "${STEP}|Terraform Destroy")" || return 1
  [[ "$out" == *$'\njob failure' && "$(grep -c '^ran ' <<<"$out")" -eq "$(grep -c '^ran \|^skipped ' <<<"$out")" ]] || return 1
  # The Cloud SQL delete fails: destroy stays skipped (it deadlocks while the
  # instance exists), so the fix above did not become an `always()` on destroy.
  out="$(simulate_destroy_gcp "$file" "$SQL_STEP")" || return 1
  [[ "$out" == *$'\nskipped Terraform Destroy\n'* && "$out" == *$'\njob failure' ]] || return 1
}

check "destroy-gcp: a failed peering delete still runs Terraform Destroy and still fails the job" \
  "$(destroy_gcp_failure_handling_rc "$CLEANUP" && echo 0 || echo 1)" \
  "simulated: none -> $(simulate_destroy_gcp "$CLEANUP" "" | tr '\n' ';') peering -> $(simulate_destroy_gcp "$CLEANUP" "$STEP" | tr '\n' ';')"

# The model, against mutants of the real workflow, so the check above cannot
# pass vacuously. Each mutant is one plausible regression.
MUTANT_DIR="$(mktemp -d)"
mutant_flagged() {
  local label="$1" program="$2" m="${MUTANT_DIR}/cleanup-staging.yml"
  awk "$program" "$CLEANUP" >"$m"
  if cmp -s "$m" "$CLEANUP"; then
    check "mutant flagged: ${label}" 1 "the mutation did not change the workflow; update the mutation"
    return
  fi
  check "mutant flagged: ${label}" "$(destroy_gcp_failure_handling_rc "$m" && echo 1 || echo 0)" \
    "the mutant passed: $(simulate_destroy_gcp "$m" "$STEP" | tr '\n' ';')"
}
# awk rather than sed: `a\` and in-place forms differ between GNU and BSD sed.
mutant_flagged "peering step without continue-on-error (destroy skipped)" \
  '$0 != "        continue-on-error: true"'
mutant_flagged "failure-reporting step removed (failure swallowed)" \
  '$0 == "      - name: Fail if the peering delete failed" { stop = 1 } !stop'
mutant_flagged "peering step id renamed (reporting step never fires)" \
  '{ if ($0 == "        id: delete-peering") $0 = "        id: peering"; print }'
mutant_flagged "reporting step on the implicit success() (silent when destroy also fails)" \
  '{ sub(/[!]cancelled[(][)] && /, ""); print }'
mutant_flagged "Terraform Destroy on always() (runs over a live Cloud SQL instance)" \
  '{ print } $0 == "      - name: Terraform Destroy" { print "        if: always()" }'
rm -rf "$MUTANT_DIR"

# --- The script, statically -------------------------------------------------
#
# Behavior below shows what the script does against stubs; these pin the
# text a stub cannot see, such as a literal network name reintroduced on a
# path the stubs never take.
awk_check "$(basename "$DELETE_SCRIPT") deletes once, on the output-named network, and removes state after" \
  "$DELETE_SCRIPT" \
  "expected: one network_name read via jq -er, one delete with --network=\"\$NETWORK\", no --async, no literal network, one state rm of \"\$PEERING_ADDR\" after the delete" '
    code_of($0) ~ /NETWORK="[$][(]jq[[:space:]]+-er[[:space:]]+.\.network_name\.value./ { reads++ }
    invokes($0, cmdre) { deletes++; if (!delete_line) delete_line = FNR }
    code_of($0) ~ /--network="[$]NETWORK"/ { by_var++ }
    code_of($0) ~ /--network=[^"]/ { literal++ }
    code_of($0) ~ /--async/ { async++ }
    code_of($0) ~ /cudly-/ { literal++ }
    code_of($0) ~ /terraform[[:space:]]+-chdir=[^[:space:]]+[[:space:]]+state[[:space:]]+rm[[:space:]]+"[$]PEERING_ADDR"[[:space:]]*$/ { rms++; rm_line = FNR }
    invokes($0, "state[[:space:]]+rm") { any_rm++ }
    END { exit !(reads == 1 && deletes == 1 && by_var == 1 && literal == 0 && async == 0 && \
                 rms == 1 && any_rm == 1 && rm_line > delete_line) }'

awk_check "$(basename "$DELETE_SCRIPT") pins the module-qualified peering address" \
  "$DELETE_SCRIPT" "expected PEERING_ADDR=\"${PEERING_ADDR}\" exactly once" '
    code_of($0) ~ /^PEERING_ADDR="module[.]networking[.]google_service_networking_connection[.]private_vpc_connection"$/ { hits++ }
    END { exit !(hits == 1) }'

awk_check "$(basename "$DELETE_SCRIPT") swallows nothing" "$DELETE_SCRIPT" \
  "found 2>/dev/null, || true or || echo in code (#330: the old step swallowed the delete)" '
    code_of($0) ~ /2>[[:space:]]*\/dev\/null/ { bad++ }
    code_of($0) ~ /[|][|][[:space:]]*(true|echo|:)/ { bad++ }
    END { exit !(bad == 0) }'

# --- Behavior: the script end to end against stubbed terraform and gcloud --
#
# Both stubs log to ONE file, so "state rm after delete" is read off a single
# call log. The gcloud stub's delete branch is an ALLOWLIST of the exact
# argument vector: `--async` (which would make the exit status meaningless) or
# any extra flag fails as behavior, not only as text.
STUB_DIR="$(mktemp -d)"
STUB_STATE="$(mktemp -d)"
CALLS="$(mktemp)"
FIXTURE_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR" "$STUB_STATE" "$FIXTURE_DIR"; rm -f "$CALLS"' EXIT
export CALLS

cat >"${STUB_DIR}/terraform" <<'EOF'
#!/usr/bin/env bash
echo "terraform $*" >>"$CALLS"
case "$2 $3" in
  "state list")
    [[ "${STATE_LIST_FAILS:-0}" == "1" ]] && { echo "state list failed" >&2; exit 1; }
    printf '%s' "$STATE_LIST"
    ;;
  "state rm")
    [[ "${STATE_RM_FAILS:-0}" == "1" ]] && exit 3
    ;;
  output*)
    printf '%s' "$TF_OUTPUT_JSON"
    ;;
esac
exit 0
EOF

cat >"${STUB_DIR}/gcloud" <<'EOF'
#!/usr/bin/env bash
echo "gcloud $*" >>"$CALLS"
if [[ "$#" -ne 7 || "$1 $2 $3" != "services vpc-peerings delete" || \
      "$4" != "--service=servicenetworking.googleapis.com" || "$5" != --network=* || \
      "$6" != --project=* || "$7" != "--quiet" ]]; then
  echo "stub: unexpected gcloud invocation: $*" >&2
  exit 99
fi
[[ "${DELETE_FAILS:-0}" == "1" ]] && { echo "delete failed" >&2; exit 254; }
exit 0
EOF
chmod +x "${STUB_DIR}/terraform" "${STUB_DIR}/gcloud"

PROJECT_OK="cudly-staging"
# Not the old literal, so a hardcoded `cudly-staging-vpc` cannot pass.
NETWORK_OK="cudly-renamed-vpc"
OUTPUTS_OK="{\"network_name\":{\"value\":\"${NETWORK_OK}\"},\"database_instance_name\":{\"value\":\"cudly-staging-postgres\"}}"
# The real address among its near-misses, as `terraform state list` prints them.
STATE_WITH_PEERING="module.networking.google_compute_network.main
module.networking.google_compute_global_address.private_ip_address
${PEERING_ADDR}
module.networking.google_vpc_access_connector.main
"

# run_script ARGS... -> STUB_EXIT, STUB_ERR, and the call log in $CALLS
run_script() {
  : >"$CALLS"
  local errfile
  errfile="$(mktemp)"
  STUB_EXIT=0
  PATH="${STUB_DIR}:${PATH}" "$DELETE_SCRIPT" "$@" </dev/null >/dev/null 2>"$errfile" || STUB_EXIT=$?
  STUB_ERR="$(cat "$errfile")"
  rm -f "$errfile"
}

count_calls() { grep -c -- "$1" "$CALLS" || true; }
log() { printf 'exit %s, calls: %s' "$STUB_EXIT" "$(tr '\n' ';' <"$CALLS")"; }

export STATE_LIST="$STATE_WITH_PEERING" TF_OUTPUT_JSON="$OUTPUTS_OK"

# The golden path, FIRST: every refusal case below is also satisfied by a
# script that never deletes anything.
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: golden path exits 0" "$([[ $STUB_EXIT -eq 0 ]] && echo 0 || echo 1)" "$(log)"
check "behavior: exactly one peering delete, on the output-named network and the given project" \
  "$([[ $(count_calls "vpc-peerings delete") -eq 1 ]] && grep -qx "gcloud services vpc-peerings delete --service=servicenetworking.googleapis.com --network=${NETWORK_OK} --project=${PROJECT_OK} --quiet" "$CALLS" && echo 0 || echo 1)" "$(log)"
check "behavior: no call names the old hardcoded network" \
  "$(! grep -q "cudly-staging-vpc" "$CALLS" && echo 0 || echo 1)" "$(log)"
check "behavior: exactly one state rm, of the exact peering address" \
  "$([[ $(count_calls "state rm") -eq 1 ]] && grep -qx "terraform -chdir=${STUB_STATE} state rm ${PEERING_ADDR}" "$CALLS" && echo 0 || echo 1)" "$(log)"
check "behavior: the state rm comes after the delete" \
  "$([[ $(grep -n "vpc-peerings delete" "$CALLS" | cut -d: -f1) -lt $(grep -n "state rm" "$CALLS" | cut -d: -f1) ]] && echo 0 || echo 1)" "$(log)"

# The #330 defect as behavior: a failed delete fails the step with gcloud's
# own status AND leaves the address in state, so a re-run retries it.
export DELETE_FAILS=1
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: a failed delete exits with gcloud's status and removes no state" \
  "$([[ $STUB_EXIT -eq 254 && $(count_calls "state rm") -eq 0 ]] && echo 0 || echo 1)" "$(log)"
check "behavior: the failed-delete error names the address left in state" \
  "$([[ "$STUB_ERR" == *"left in state"* && "$STUB_ERR" == *"$PEERING_ADDR"* ]] && echo 0 || echo 1)" "stderr: ${STUB_ERR}"
unset DELETE_FAILS

export STATE_RM_FAILS=1
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: a failed state rm fails the step after exactly one delete" \
  "$([[ $STUB_EXIT -ne 0 && $(count_calls "vpc-peerings delete") -eq 1 ]] && echo 0 || echo 1)" "$(log)"
unset STATE_RM_FAILS

# Not in state: a re-run after a completed cleanup. Near-misses of the address
# (prefix, suffix, other module, leading space) must not count as present.
export STATE_LIST="module.networking.google_compute_network.main
${PEERING_ADDR}_old
module.networking_old.google_service_networking_connection.private_vpc_connection
 ${PEERING_ADDR}
module.networking.google_service_networking_connection.private_vpc_connection[0]
"
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: peering not in state (only near-miss addresses) exits 0, deletes nothing, removes nothing" \
  "$([[ $STUB_EXIT -eq 0 && $(count_calls "^gcloud") -eq 0 && $(count_calls "state rm") -eq 0 ]] && echo 0 || echo 1)" "$(log)"

export STATE_LIST=""
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: an empty state exits 0 without calling gcloud" \
  "$([[ $STUB_EXIT -eq 0 && $(count_calls "^gcloud") -eq 0 ]] && echo 0 || echo 1)" "$(log)"

# The address on an unterminated final line is still found.
export STATE_LIST="module.networking.google_compute_network.main
${PEERING_ADDR}"
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: the address on an unterminated last line is still deleted" \
  "$([[ $STUB_EXIT -eq 0 && $(count_calls "vpc-peerings delete") -eq 1 ]] && echo 0 || echo 1)" "$(log)"

export STATE_LIST="$STATE_WITH_PEERING" STATE_LIST_FAILS=1
run_script "$STUB_STATE" "$PROJECT_OK"
check "behavior: an unreadable state fails the step without calling gcloud" \
  "$([[ $STUB_EXIT -ne 0 && $(count_calls "^gcloud") -eq 0 && $(count_calls "state rm") -eq 0 ]] && echo 0 || echo 1)" "$(log)"
unset STATE_LIST_FAILS

# In state, but the network cannot be named: refuse, and leave state alone.
# Each value is its own failure mode; the last one is flag-shaped.
while IFS='|' read -r label outputs; do
  [[ -n "$label" ]] || continue
  export TF_OUTPUT_JSON="$outputs"
  run_script "$STUB_STATE" "$PROJECT_OK"
  check "behavior: ${label} fails without calling gcloud or state rm" \
    "$([[ $STUB_EXIT -ne 0 && $(count_calls "^gcloud") -eq 0 && $(count_calls "state rm") -eq 0 ]] && echo 0 || echo 1)" "$(log)"
done <<'EOF'
non-JSON terraform output|Warning: something
a JSON array as terraform output|[]
no outputs at all|{}
network_name absent|{"database_instance_name":{"value":"cudly-staging-postgres"}}
network_name null|{"network_name":{"value":null}}
network_name empty|{"network_name":{"value":""}}
network_name with whitespace|{"network_name":{"value":"cudly staging vpc"}}
network_name upper case|{"network_name":{"value":"Cudly-Staging-VPC"}}
network_name flag-shaped|{"network_name":{"value":"--project=other"}}
EOF
export TF_OUTPUT_JSON="$OUTPUTS_OK"

for bad_project in "" "  " "Cudly-Staging" "cudly"; do
  run_script "$STUB_STATE" "$bad_project"
  check "behavior: malformed project '${bad_project}' exits 2 without calling terraform or gcloud" \
    "$([[ $STUB_EXIT -eq 2 && ! -s "$CALLS" ]] && echo 0 || echo 1)" "$(log)"
done

run_script
check "behavior: no arguments exits 2" "$([[ $STUB_EXIT -eq 2 ]] && echo 0 || echo 1)" "$(log)"
run_script "$STUB_STATE" "$PROJECT_OK" extra
check "behavior: three arguments exits 2" "$([[ $STUB_EXIT -eq 2 ]] && echo 0 || echo 1)" "$(log)"
run_script "${STUB_STATE}/does-not-exist" "$PROJECT_OK"
check "behavior: a missing state directory exits 2" "$([[ $STUB_EXIT -eq 2 ]] && echo 0 || echo 1)" "$(log)"

# --- Sweep: the script is the only place that deletes a peering -------------
#
# Keyed on the dangerous command, not on a file name, over every workflow and
# a recursive glob of scripts/, so a new inline delete (the #330 shape) is
# caught wherever it is added.

# sweep_sites FILE... -> one "FILE:LINE" per invocation, then "total N"
sweep_sites() {
  awk -v SQ="'" -v cmdre="$DELETE_CMD_RE" "$AWK_CODE_FUNCS"'
    invokes($0, cmdre) { print FILENAME ":" FNR; total++ }
    END { print "total " total + 0 }' "$@"
}

# assert_sweep LABEL EXPECTED_SITES_EXCEPT_SCRIPT EXPECTED_TOTAL FILE...
assert_sweep() {
  local label="$1" expected_other="$2" expected_total="$3" report other
  shift 3
  report="$(sweep_sites "$@")"
  # `|| true` only on grep, whose exit 1 means "no other site": the clean case.
  other="$({ grep -v -e "^total " -e "^${DELETE_SCRIPT}:" <<<"$report" || true; } | sed 's/:[0-9]*$//' | sort -u | tr '\n' ' ')"
  check "$label" \
    "$([[ "$other" == "$expected_other" && "$report" == *"total ${expected_total}" ]] && echo 0 || echo 1)" \
    "expected other sites '${expected_other}' and total ${expected_total}, got: $(tr '\n' ' ' <<<"$report")"
}

build_swept_scripts "$SCRIPT_DIR"
shopt -s nullglob
WORKFLOWS=("${WORKFLOW_DIR}"/*.yml "${WORKFLOW_DIR}"/*.yaml)
shopt -u nullglob

swept_has_script=1
# `${arr[@]+...}`: bash 3.2 (macOS) treats an empty "${arr[@]}" as unbound under set -u.
for f in ${SWEPT_SCRIPTS[@]+"${SWEPT_SCRIPTS[@]}"}; do [[ "$f" == "$DELETE_SCRIPT" ]] && swept_has_script=0; done
check "the swept set holds workflows and includes $(basename "$DELETE_SCRIPT")" \
  "$([[ ${#WORKFLOWS[@]} -gt 0 && $swept_has_script -eq 0 ]] && echo 0 || echo 1)" \
  "${#WORKFLOWS[@]} workflow(s); an empty swept set has no violations either"

# Exactly one site in the whole tree, and it is the script: zero would mean the
# sweep stopped recognizing the command, which passes vacuously.
assert_sweep "every 'gcloud services vpc-peerings delete' in .github/workflows and scripts/ is the script's" \
  "" 1 ${WORKFLOWS[@]+"${WORKFLOWS[@]}"} ${SWEPT_SCRIPTS[@]+"${SWEPT_SCRIPTS[@]}"}

# The sweep itself, over fixtures. The unwired one is the pre-fix step from
# origin/main with its two comment lines dropped.
cat >"${FIXTURE_DIR}/unwired.yml" <<'EOF'
      - name: Delete the Cloud SQL instance this state owns
        run: |
          ./scripts/delete-owned-cloud-sql-instance.sh terraform/environments/gcp "$PROJECT"
          cd terraform/environments/gcp
          terraform state rm module.networking.google_service_networking_connection.private_vpc_connection 2>/dev/null || true
          gcloud services vpc-peerings delete \
            --service=servicenetworking.googleapis.com \
            --network=cudly-staging-vpc \
            --project="$PROJECT" --quiet 2>/dev/null || echo "VPC peering already gone or not found"
EOF
cat >"${FIXTURE_DIR}/prose.yml" <<'EOF'
      - name: Mentions the command without running it
        run: |
          # every `gcloud services vpc-peerings delete` goes through the script
          echo "gcloud services vpc-peerings delete is not run here"
EOF

assert_sweep "an inline peering delete (the pre-fix step) is flagged" \
  "${FIXTURE_DIR}/unwired.yml " 1 "${FIXTURE_DIR}/unwired.yml"
assert_sweep "a step that only mentions the command is not a delete site" \
  "" 0 "${FIXTURE_DIR}/prose.yml"

echo
echo "passed: ${pass}, failed: ${fail}"
[[ "$fail" -eq 0 ]]
