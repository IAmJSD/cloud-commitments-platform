#!/usr/bin/env bash
# shellcheck disable=SC2016  # expected findings quote workflow text, ${{ }} included
# test-tfstate-concurrency-group.sh
#
# Asserts that every workflow job which WRITES the Azure or GCP Terraform state
# is serialized by the shared concurrency group for that state, and that the
# group's environment suffix comes from the same literal or `${{ }}` expression
# as the state key the job writes (#195).
#
#   Azure  key    = "github-<env>.terraform.tfstate"  group: azure-tfstate-<env>
#   GCP    prefix = "github-<env>"                    group: gcp-tfstate-<env>
#
# The group is a hand-written string in every writer (deploy-azure.yml,
# cleanup-staging.yml and rollback.yml for Azure; deploy-gcp.yml,
# cleanup-staging.yml and rollback.yml for GCP). Nothing in GitHub ties it to
# the state object: a fourth writer added without the group, a typo in the
# prefix, or a suffix built from a different input than the key all run green
# and simply stop serializing. The damage shows up later as missing state
# entries, the silent failure behind cloud-commitments-cli#1801. A green
# workflow run proves nothing about it, so the pairing is asserted as text.
#
# The AWS state objects have their own guard, test-aws-tfstate-platform-key.sh,
# because they add a compute_platform split this one does not need.
#
# What counts as a writer. A job belongs to a state when it builds that state's
# key (anywhere in the job, not only in a `key =` line) or names its root
# directory, terraform/environments/{azure,gcp}. It WRITES that state when it
# runs any terraform subcommand outside a read-only allowlist (init without
# -migrate-state/-force-copy, plan, output, show, validate, state
# list/show/pull, ...), or runs a script under scripts/ that does. Unknown
# subcommands count as writes, so the check fails
# closed: a job that only reads state (database-migration.yml's `terraform
# output`) needs no group, and one that does anything else needs the right one.
#
# What "derived from the same expression" means. The environment in the key is
# traced to a literal or a `${{ }}` expression: directly, through the printf
# argument that fills the key's `%s`, and through the step's or job's `env:`
# (or a shell assignment in the same step). The group suffix must be that same
# literal or expression, compared with whitespace removed. Anything the trace
# cannot resolve is reported rather than passed.
#
# A writer's group must not cancel a run in progress either: cancel-in-progress
# on its job-level group (and on a workflow-level group, if any) must be absent
# or `false`, or a newer queued run would cancel a terraform write mid-run.
#
# Both directions are asserted, and the positive one first: a scan that
# recognizes no writer has no violations either, and would pass while every
# group in the repository was wrong.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKFLOW_DIR="${REPO_ROOT}/.github/workflows"

# code_of() and build_swept_scripts(), shared with the other workflow guard
# suites so the rule separating code from prose has one definition.
# shellcheck source=scripts/lib/code-scan-awk.sh
. "${SCRIPT_DIR}/lib/code-scan-awk.sh"

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

# The scanner: TF_WRITE_FUNCS, mutating_scripts and scan_state_groups.
# shellcheck source=scripts/lib/tfstate-group-scan.sh
. "${SCRIPT_DIR}/lib/tfstate-group-scan.sh"

# assert_scan LABEL EXPECTED DIR [SCRIPTS_DIR]
#
# EXPECTED empty asserts the scan finds nothing; otherwise it asserts EXPECTED
# appears in the report, so a fixture pins WHICH job was flagged and why rather
# than only that something was. An awk syntax error also produces a non-empty
# report, and matching the text is what tells the two apart.
assert_scan() {
  local label="$1"
  local expected="$2"
  local dir="$3"
  local scripts_dir="${4:-${FIXTURE_DIR}/no-scripts}"
  local report line

  report="$(scan_state_groups violations "$dir" "$scripts_dir")"

  if [[ -z "$expected" && -z "$report" ]] || [[ -n "$expected" && "$report" == *"$expected"* ]]; then
    note_pass "$label"
  else
    note_fail "$label"
    if [[ -z "$expected" ]]; then
      echo "      expected no findings, got:"
    else
      echo "      expected a finding containing '${expected}', got:"
    fi
    if [[ -z "$report" ]]; then
      echo "        (no findings)"
    else
      while IFS= read -r line; do
        echo "        ${line}"
      done <<<"$report"
    fi
  fi
}

FIXTURE_DIR="$(mktemp -d)"
trap 'rm -rf "$FIXTURE_DIR"' EXIT
mkdir -p "${FIXTURE_DIR}/no-scripts"

# --- Positive direction FIRST: the real writers are seen and are right --------
#
# Every job that touches the Azure or GCP state today, with the environment its
# key resolves to, the group it carries and that group's cancel-in-progress
# (pinned as read, so an unparsed `false` cannot pass as an absent one). Named explicitly and asserted BEFORE
# any absence: if the scanner stops recognizing these, the sweep below reports a
# clean result for a repository it no longer understands. The two readers are
# pinned too, so the read/write split is asserted rather than assumed.
EXPECTED_SITES=$(
  cat <<'EOF'
deploy-azure.yml|build-and-deploy|azure|expr:needs.prepare.outputs.target_environment|writes|azure-tfstate-${{needs.prepare.outputs.target_environment}}|false
cleanup-staging.yml|destroy-azure|azure|lit:staging|writes|azure-tfstate-staging|false
rollback.yml|rollback-azure|azure|expr:inputs.environment|writes|azure-tfstate-${{inputs.environment}}|false
deploy-gcp.yml|build-and-deploy|gcp|expr:needs.prepare.outputs.target_environment|writes|gcp-tfstate-${{needs.prepare.outputs.target_environment}}|false
cleanup-staging.yml|destroy-gcp|gcp|lit:staging|writes|gcp-tfstate-staging|false
rollback.yml|rollback-gcp|gcp|expr:inputs.environment|writes|gcp-tfstate-${{inputs.environment}}|false
database-migration.yml|migrate-azure|azure|expr:inputs.environment|reads|db-migrate-azure-${{inputs.environment||'unset'}}|false
database-migration.yml|migrate-gcp|gcp|expr:inputs.environment|reads|db-migrate-gcp-${{inputs.environment||'unset'}}|false
EOF
)

CENSUS="$(scan_state_groups census "$WORKFLOW_DIR" "$SCRIPT_DIR")"

# Counted by the census shape rather than by line, so the scanner's own error
# lines ("no workflow files found under ...") are not counted as jobs.
CENSUS_WRITERS=$(printf '%s\n' "$CENSUS" | grep -cE '^[^|]+\|[^|]*\|[^|]+\|[^|]+\|writes\|[^|]+\|[^|]+$' || true)

if [[ "$CENSUS_WRITERS" -eq 0 ]]; then
  note_fail "the scan recognizes no Azure or GCP state writer under ${WORKFLOW_DIR}"
  echo "      a scan that recognizes nothing has no violations either, so every"
  echo "      assertion below would pass over a repository it never read"
else
  note_pass "the scan recognizes ${CENSUS_WRITERS} job(s) writing Azure or GCP state"
fi

while IFS= read -r expected_site; do
  [[ -z "$expected_site" ]] && continue
  if printf '%s\n' "$CENSUS" | grep -Fxq "$expected_site"; then
    note_pass "${expected_site} is recognized as expected"
  else
    note_fail "expected site not found: ${expected_site}"
    echo "      format is file|job|state|key-environment|reads-or-writes|concurrency-group|cancel-in-progress;"
    echo "      the census holds:"
    if [[ -z "$CENSUS" ]]; then
      echo "        (nothing)"
    else
      while IFS= read -r line; do
        echo "        ${line}"
      done <<<"$CENSUS"
    fi
  fi
done <<<"$EXPECTED_SITES"

# --- Negative direction: no writer anywhere is unserialized or mis-grouped ----
#
# Keyed on what a job does, over a GLOB of .github/workflows (both extensions
# GitHub accepts), so a fourth writer added later is covered without anyone
# remembering to list it. That is the realistic regression #195 names.
assert_scan "every Azure and GCP state writer carries the group derived from its key" \
  "" "$WORKFLOW_DIR" "$SCRIPT_DIR"

# The same sweep with the real tree mutated the three ways #195 describes. Each
# mutation is applied to a copy, and each is first checked to have changed the
# copy, so a mutation that stops matching the workflow text fails here instead
# of passing an unmodified tree.
mutate_real() {
  local name="$1" file="$2" from="$3" to="$4"
  mkdir -p "${FIXTURE_DIR}/real-${name}"
  cp "${WORKFLOW_DIR}"/*.yml "${FIXTURE_DIR}/real-${name}/"
  FROM="$from" TO="$to" awk '{ i = index($0, ENVIRON["FROM"]); if (i) $0 = substr($0, 1, i - 1) ENVIRON["TO"] substr($0, i + length(ENVIRON["FROM"])); print }' \
    "${WORKFLOW_DIR}/${file}" >"${FIXTURE_DIR}/real-${name}/${file}"
  if cmp -s "${WORKFLOW_DIR}/${file}" "${FIXTURE_DIR}/real-${name}/${file}"; then
    note_fail "mutation '${name}' did not change ${file}; update the mutation text"
  fi
}

mutate_real nogroup rollback.yml 'group: azure-tfstate-${{ inputs.environment }}' '# group removed'
mutate_real typo deploy-azure.yml 'group: azure-tfstate-' 'group: azure-tfsate-'
mutate_real suffix cleanup-staging.yml 'group: gcp-tfstate-staging' 'group: gcp-tfstate-${{ inputs.environment }}'
mutate_real cancel rollback.yml 'cancel-in-progress: false' 'cancel-in-progress: true'

assert_scan "the real rollback-azure without its group is flagged" \
  'rollback.yml: job "rollback-azure": runs terraform apply against the azure Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/real-nogroup" "$SCRIPT_DIR"

assert_scan "a typo in the real deploy-azure group prefix is flagged" \
  'deploy-azure.yml: job "build-and-deploy": runs terraform apply against the azure Terraform state, but its concurrency group azure-tfsate-${{needs.prepare.outputs.target_environment}} is not in the azure-tfstate-* family' \
  "${FIXTURE_DIR}/real-typo" "$SCRIPT_DIR"

assert_scan "a real destroy-gcp group suffix not derived from its key is flagged" \
  'cleanup-staging.yml: job "destroy-gcp": runs scripts/delete-owned-cloud-sql-instance.sh against the gcp Terraform state, but its concurrency group gcp-tfstate-${{inputs.environment}} is not derived from the same value as the state key (lit:staging)' \
  "${FIXTURE_DIR}/real-suffix" "$SCRIPT_DIR"

assert_scan "the real rollback-azure group set to cancel-in-progress: true is flagged" \
  'rollback.yml: job "rollback-azure": runs terraform apply against the azure Terraform state, but its concurrency group sets cancel-in-progress: true' \
  "${FIXTURE_DIR}/real-cancel" "$SCRIPT_DIR"

# --- The scanner itself, over fixtures ----------------------------------------
mkdir -p "${FIXTURE_DIR}/ok" "${FIXTURE_DIR}/fourth" "${FIXTURE_DIR}/family" \
  "${FIXTURE_DIR}/drift" "${FIXTURE_DIR}/untraced" "${FIXTURE_DIR}/viascript/wf" \
  "${FIXTURE_DIR}/viascript/scripts/lib" "${FIXTURE_DIR}/nokey" \
  "${FIXTURE_DIR}/misattrib" "${FIXTURE_DIR}/readers" "${FIXTURE_DIR}/mixed"

# Every shape the real writers use, plus the variants a refactor would produce:
# key and apply in different steps, env declared after run, the env set at job
# level, the shorthand `concurrency: <group>`, a .yaml file, a prose comment
# naming a write, and a reader with no group.
cat >"${FIXTURE_DIR}/ok/deploy.yml" <<'EOF'
jobs:
  deploy-azure:
    concurrency:
      group: azure-tfstate-${{ needs.prepare.outputs.target_environment }}
      cancel-in-progress: false
    steps:
      - name: Write Terraform backend config
        env:
          TF_BACKEND: ${{ secrets.TF_BACKEND_AZURE }}
          ENVIRONMENT: ${{needs.prepare.outputs.target_environment}}
        run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
      - name: Terraform Apply
        run: |
          cd terraform/environments/azure
          terraform apply -auto-approve tfplan
  deploy-gcp:
    concurrency: gcp-tfstate-${{ inputs.environment }}
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - name: Terraform Init
        run: |
          printf '%s\nprefix = "github-%s"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform -chdir=terraform/environments/gcp destroy -auto-approve
        env:
          TF_BACKEND: ${{ secrets.TF_BACKEND_GCP }}
EOF

cat >"${FIXTURE_DIR}/ok/reader.yaml" <<'EOF'
jobs:
  migrate-azure:
    # A reader: terraform apply is named in this comment only.
    concurrency:
      group: db-migrate-azure-${{ inputs.environment }}
    steps:
      - name: Read the endpoint
        env:
          ENVIRONMENT: ${{ inputs.environment }}
        run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          cd terraform/environments/azure
          terraform init -backend-config=/tmp/backend.tfbackend
          DB_ENDPOINT=$(terraform output -raw database_server_fqdn)
          terraform -chdir=. state list
  destroy-azure:
    concurrency:
      group: "azure-tfstate-staging"
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform destroy -auto-approve
EOF

assert_scan "every writer shape the workflows use, and a reader, pass" \
  "" "${FIXTURE_DIR}/ok"

# The regression #195 names: a fourth writer of the Azure blob with no group.
cat >"${FIXTURE_DIR}/fourth/import.yml" <<'EOF'
jobs:
  import-azure:
    steps:
      - name: Import
        env:
          ENVIRONMENT: ${{ inputs.environment }}
        run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          cd terraform/environments/azure
          terraform import azurerm_resource_group.main /subscriptions/x
EOF

assert_scan "a fourth Azure writer with no group is flagged" \
  'import.yml: job "import-azure": runs terraform import against the azure Terraform state with no concurrency group; expected azure-tfstate-<environment>' \
  "${FIXTURE_DIR}/fourth"

# A typo in the prefix, and a GCP writer carrying the Azure group.
cat >"${FIXTURE_DIR}/family/wrong.yml" <<'EOF'
jobs:
  rollback-azure:
    concurrency:
      group: azure-tfstate${{ inputs.environment }}
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
  rollback-gcp:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%s\nprefix = "github-%s"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform state rm google_sql_user.x
EOF

assert_scan "a typo in the Azure group prefix is flagged" \
  'wrong.yml: job "rollback-azure": runs terraform apply against the azure Terraform state, but its concurrency group azure-tfstate${{inputs.environment}} is not in the azure-tfstate-* family' \
  "${FIXTURE_DIR}/family"

assert_scan "a GCP writer carrying the Azure group is flagged" \
  'wrong.yml: job "rollback-gcp": runs terraform state rm against the gcp Terraform state, but its concurrency group azure-tfstate-${{inputs.environment}} is not in the gcp-tfstate-* family' \
  "${FIXTURE_DIR}/family"

# The right family with a suffix from a different value than the key: a literal
# that disagrees, and an expression from a different input.
cat >"${FIXTURE_DIR}/drift/drift.yml" <<'EOF'
jobs:
  destroy-azure:
    concurrency:
      group: azure-tfstate-staging
    steps:
      - run: |
          printf '%s\nkey = "github-dev.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform destroy -auto-approve
  deploy-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    steps:
      - env:
          ENVIRONMENT: ${{ needs.prepare.outputs.target_environment }}
        run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF

assert_scan "a literal group suffix disagreeing with a literal key is flagged" \
  'drift.yml: job "destroy-azure": runs terraform destroy against the azure Terraform state, but its concurrency group azure-tfstate-staging is not derived from the same value as the state key (lit:dev)' \
  "${FIXTURE_DIR}/drift"

assert_scan "a group suffix from a different expression than the key is flagged" \
  'drift.yml: job "deploy-azure": runs terraform apply against the azure Terraform state, but its concurrency group azure-tfstate-${{inputs.environment}} is not derived from the same value as the state key (expr:needs.prepare.outputs.target_environment)' \
  "${FIXTURE_DIR}/drift"

# A key whose environment cannot be traced is reported, not passed. The env set
# in an EARLIER step does not reach this one, exactly as on a runner.
cat >"${FIXTURE_DIR}/untraced/untraced.yml" <<'EOF'
jobs:
  rollback-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    steps:
      - name: Earlier step
        env:
          ENVIRONMENT: ${{ inputs.environment }}
        run: echo "$ENVIRONMENT"
      - name: Rollback
        run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF

assert_scan "a key environment that does not trace to this step is reported" \
  'untraced.yml: job "rollback-azure": runs terraform apply against the azure Terraform state, and the environment in its state key cannot be traced to a literal or a ${{ }} expression' \
  "${FIXTURE_DIR}/untraced"

# A writer that delegates the write to a script under scripts/, at any depth.
cat >"${FIXTURE_DIR}/viascript/wf/destroy.yml" <<'EOF'
jobs:
  destroy-gcp:
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform init -backend-config=/tmp/backend.tfbackend
          ./scripts/lib/drop-from-state.sh terraform/environments/gcp
EOF
cat >"${FIXTURE_DIR}/viascript/scripts/lib/drop-from-state.sh" <<'EOF'
#!/usr/bin/env bash
X="$(terraform -chdir="$1" state rm module.networking.thing)"
EOF

assert_scan "a writer that delegates to a script that writes state is flagged" \
  'destroy.yml: job "destroy-gcp": runs scripts/lib/drop-from-state.sh against the gcp Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/viascript/wf" "${FIXTURE_DIR}/viascript/scripts"

# A writer in the Azure root that builds no recognizable key: its group cannot
# be checked against anything, so it is reported. An unparseable subcommand
# counts as a write.
cat >"${FIXTURE_DIR}/nokey/nokey.yml" <<'EOF'
jobs:
  apply-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    steps:
      - run: |
          cd terraform/environments/azure/
          terraform \
            apply -auto-approve
EOF

assert_scan "a writer in the Azure root with no state key is reported" \
  'nokey.yml: job "apply-azure": runs terraform (unparsed) against the azure Terraform state but builds no azure state key in the job' \
  "${FIXTURE_DIR}/nokey"

cat >"${FIXTURE_DIR}/mixed/mixed.yml" <<'EOF'
jobs:
  destroy-everything:
    concurrency:
      group: azure-tfstate-staging
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$A" > /tmp/a.tfbackend
          printf '%s\nprefix = "github-staging"\n' "$G" > /tmp/g.tfbackend
          terraform destroy -auto-approve
EOF

assert_scan "a job touching both states is flagged" \
  'mixed.yml: job "destroy-everything": touches both the Azure and the GCP Terraform state' \
  "${FIXTURE_DIR}/mixed"

# A site running to the end of its file is flushed by the next file, so the
# report must name the file the site came from.
cat >"${FIXTURE_DIR}/misattrib/a-bad.yml" <<'EOF'
jobs:
  rollback-azure:
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF
cat >"${FIXTURE_DIR}/misattrib/b-innocent.yml" <<'EOF'
jobs:
  summary:
    steps:
      - run: echo "clean"
EOF

assert_scan "a finding names the file it came from, not the file scanned after it" \
  'a-bad.yml: job "rollback-azure"' "${FIXTURE_DIR}/misattrib"

# Readers only: not violations, but also not evidence of a clean repository.
cat >"${FIXTURE_DIR}/readers/read.yml" <<'EOF'
jobs:
  migrate-gcp:
    steps:
      - run: |
          printf '%s\nprefix = "github-%s"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform plan
EOF

assert_scan "a directory holding no writer says so rather than passing" \
  'no job writing the Azure or GCP Terraform state found at all' "${FIXTURE_DIR}/readers"

assert_scan "a directory that does not exist is reported, not passed" \
  'no workflow files found under' "${FIXTURE_DIR}/does-not-exist"

# Fixtures for the cases review raised (#1149), kept in their own file so each
# file stays under the 500-line limit. It uses assert_scan and FIXTURE_DIR.
# shellcheck source=scripts/lib/tfstate-group-edge-cases.sh
. "${SCRIPT_DIR}/lib/tfstate-group-edge-cases.sh"

echo
echo "passed: ${pass}, failed: ${fail}"
[[ "$fail" -eq 0 ]]
