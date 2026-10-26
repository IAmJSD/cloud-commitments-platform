#!/usr/bin/env bash
# shellcheck disable=SC2016  # expected findings quote workflow text, ${{ }} included
# tfstate-group-edge-cases.sh
#
# Fixtures for scripts/test-tfstate-concurrency-group.sh (#195) covering the
# false passes review found on #1149. Sourced by that suite after its own
# fixtures, not executed: it uses the suite's assert_scan and FIXTURE_DIR, and
# its results count towards the suite's pass/fail totals.

# --- cancel-in-progress ------------------------------------------------------
#
# The right group is not enough if it cancels the run holding it: a newer run
# queued on azure-tfstate-<env> would cancel a terraform apply mid-run, leaving
# a held blob lease or half-applied state. Every real writer sets `false`, so
# anything other than `false` or no setting at all is reported. An expression
# can evaluate to true, so it is reported too.
mkdir -p "${FIXTURE_DIR}/cancel" "${FIXTURE_DIR}/cancel-wf" "${FIXTURE_DIR}/cancel-reader"

cat >"${FIXTURE_DIR}/cancel/cancel.yml" <<'EOF'
jobs:
  rollback-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
      cancel-in-progress: true
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
  rollback-gcp:
    concurrency:
      cancel-in-progress: ${{ github.event_name == 'push' }}
      group: gcp-tfstate-${{ inputs.environment }}
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%s\nprefix = "github-%s"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform destroy -auto-approve
EOF

assert_scan "a writer whose group sets cancel-in-progress: true is flagged" \
  'cancel.yml: job "rollback-azure": runs terraform apply against the azure Terraform state, but its concurrency group sets cancel-in-progress: true; a newer run queued on the group would cancel the write mid-run' \
  "${FIXTURE_DIR}/cancel"

assert_scan "a writer whose cancel-in-progress is an expression is flagged" \
  'cancel.yml: job "rollback-gcp": runs terraform destroy against the gcp Terraform state, but its concurrency group sets cancel-in-progress: ${{github.event_name=='"'push'"'}}' \
  "${FIXTURE_DIR}/cancel"

# The workflow-level group cancels the whole run, writer job included, so a
# right job-level group does not make up for it.
cat >"${FIXTURE_DIR}/cancel-wf/deploy.yml" <<'EOF'
concurrency:
  group: deploy-${{ github.ref }}
  cancel-in-progress: true
jobs:
  deploy-azure:
    concurrency:
      group: azure-tfstate-staging
      cancel-in-progress: false
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF

assert_scan "a writer in a workflow whose own group cancels runs is flagged" \
  'deploy.yml: job "deploy-azure": runs terraform apply against the azure Terraform state, but the workflow-level concurrency sets cancel-in-progress: true' \
  "${FIXTURE_DIR}/cancel-wf"

# Readers are not held to it, and an explicit `false` on a writer passes. The
# writer is there so the directory is not reported as holding no writer.
cat >"${FIXTURE_DIR}/cancel-reader/read.yml" <<'EOF'
jobs:
  migrate-gcp:
    concurrency:
      group: db-migrate-gcp-${{ inputs.environment }}
      cancel-in-progress: true
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform init -backend-config=/tmp/backend.tfbackend -reconfigure
          terraform output -raw database_host
  destroy-gcp:
    concurrency:
      group: gcp-tfstate-staging
      cancel-in-progress: "false"
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform destroy -auto-approve
EOF

assert_scan "a reader that cancels, and a writer with cancel-in-progress false, pass" \
  "" "${FIXTURE_DIR}/cancel-reader"

# --- terraform init that migrates state -------------------------------------
#
# init is read-only unless it copies state into the backend it configures.
mkdir -p "${FIXTURE_DIR}/init-migrate"

cat >"${FIXTURE_DIR}/init-migrate/migrate.yml" <<'EOF'
jobs:
  migrate-azure-backend:
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          cd terraform/environments/azure
          terraform init -backend-config=/tmp/backend.tfbackend -migrate-state -force-copy
  migrate-gcp-backend:
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform -chdir=terraform/environments/gcp init -force-copy=true && echo done
  migrate-gcp-continued:
    concurrency:
      group: gcp-tfstate-dev
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform -chdir=terraform/environments/gcp init \
            -migrate-state
EOF

assert_scan "terraform init -migrate-state with no group is flagged" \
  'migrate.yml: job "migrate-azure-backend": runs terraform init -migrate-state against the azure Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/init-migrate"

assert_scan "terraform init -force-copy=true with no group is flagged" \
  'migrate.yml: job "migrate-gcp-backend": runs terraform init -force-copy against the gcp Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/init-migrate"

assert_scan "terraform init with flags continued onto the next line counts as a write" \
  'migrate.yml: job "migrate-gcp-continued": runs terraform init (unparsed) against the gcp Terraform state, but its concurrency group gcp-tfstate-dev is not derived from the same value as the state key (lit:staging)' \
  "${FIXTURE_DIR}/init-migrate"

# --- Job and step boundaries in other YAML styles ----------------------------
#
# A quoted job id is still a job: its lines must not merge into the job before
# it and borrow that job's group.
mkdir -p "${FIXTURE_DIR}/quoted-id" "${FIXTURE_DIR}/steps4" "${FIXTURE_DIR}/steps4-ok"

cat >"${FIXTURE_DIR}/quoted-id/quoted.yml" <<'EOF'
jobs:
  good-azure:
    concurrency:
      group: azure-tfstate-staging
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform apply -auto-approve
  "bad-azure":
    steps:
      - run: |
          printf '%s\nkey = "github-staging.terraform.tfstate"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF

assert_scan "a writer under a quoted job id is a job of its own" \
  'quoted.yml: job "bad-azure": runs terraform apply against the azure Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/quoted-id"

# Steps written as a 4-space sequence: the env of an earlier step must still
# not reach a later step's key, even when `- env:` sits at the job-key indent.
cat >"${FIXTURE_DIR}/steps4/steps4.yml" <<'EOF'
jobs:
  rollback-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    steps:
    - env:
        ENVIRONMENT: ${{ inputs.environment }}
      name: Earlier step, opening with env
      run: echo "$ENVIRONMENT"
    - name: Rollback
      run: |
        printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
        terraform apply -auto-approve
EOF

assert_scan "a 4-space step sequence still scopes env: to its own step" \
  'steps4.yml: job "rollback-azure": runs terraform apply against the azure Terraform state, and the environment in its state key cannot be traced' \
  "${FIXTURE_DIR}/steps4"

# The same style with the env in the step that builds the key resolves, so the
# finding above comes from the step boundary and not from the style.
cat >"${FIXTURE_DIR}/steps4-ok/steps4.yml" <<'EOF'
jobs:
  rollback-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.environment }}
    steps:
    - name: Rollback
      env:
        ENVIRONMENT: ${{ inputs.environment }}
      run: |
        printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
        terraform apply -auto-approve
EOF

assert_scan "a 4-space step sequence with the env in the key's step passes" \
  "" "${FIXTURE_DIR}/steps4-ok"

# A step that opens with `- env:` ends its env block at the next step key, so a
# `NAME: value` line inside the run script (here YAML written by a heredoc) is
# not read as an env entry that resolves the key.
mkdir -p "${FIXTURE_DIR}/env-scope"
cat >"${FIXTURE_DIR}/env-scope/heredoc.yml" <<'EOF'
jobs:
  destroy-azure:
    concurrency:
      group: azure-tfstate-staging
    steps:
      - env:
          TF_BACKEND: ${{ secrets.TF_BACKEND_AZURE }}
        run: |
          cat > /tmp/values.yaml <<'VALUES'
          ENVIRONMENT: staging
          VALUES
          printf '%s\nkey = "github-%s.terraform.tfstate"\n' "$TF_BACKEND" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform destroy -auto-approve
EOF

assert_scan "a NAME: value line in a run script is not an env entry" \
  'heredoc.yml: job "destroy-azure": runs terraform destroy against the azure Terraform state, and the environment in its state key cannot be traced' \
  "${FIXTURE_DIR}/env-scope"

# --- Tracing the key's environment through printf -----------------------------
#
# Every conversion before the key takes an argument, not only %s. A * width
# takes one more, so its key is reported as untraceable.
mkdir -p "${FIXTURE_DIR}/printf"

cat >"${FIXTURE_DIR}/printf/printf.yml" <<'EOF'
jobs:
  deploy-azure:
    concurrency:
      group: azure-tfstate-${{ inputs.count }}
    env:
      COUNT: ${{ inputs.count }}
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%d%% done\nkey = "github-%s.terraform.tfstate"\n' "$COUNT" "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
  deploy-gcp:
    concurrency:
      group: gcp-tfstate-${{ inputs.environment }}
    env:
      ENVIRONMENT: ${{ inputs.environment }}
    steps:
      - run: |
          printf '%*s\nprefix = "github-%s"\n' 4 x "$ENVIRONMENT" > /tmp/backend.tfbackend
          terraform apply -auto-approve
EOF

assert_scan "a %d before the key does not shift the traced argument" \
  'printf.yml: job "deploy-azure": runs terraform apply against the azure Terraform state, but its concurrency group azure-tfstate-${{inputs.count}} is not derived from the same value as the state key (expr:inputs.environment)' \
  "${FIXTURE_DIR}/printf"

assert_scan "a * width before the key is reported as untraceable" \
  'printf.yml: job "deploy-gcp": runs terraform apply against the gcp Terraform state, and the environment in its state key cannot be traced' \
  "${FIXTURE_DIR}/printf"

# --- Delegation through more than one script ---------------------------------
mkdir -p "${FIXTURE_DIR}/nested/wf" "${FIXTURE_DIR}/nested/scripts/lib"

cat >"${FIXTURE_DIR}/nested/wf/destroy.yml" <<'EOF'
jobs:
  destroy-gcp:
    steps:
      - run: |
          printf '%s\nprefix = "github-staging"\n' "$TF_BACKEND" > /tmp/backend.tfbackend
          ./scripts/outer.sh terraform/environments/gcp
EOF
cat >"${FIXTURE_DIR}/nested/scripts/outer.sh" <<'EOF'
#!/usr/bin/env bash
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"${SCRIPT_DIR}/lib/middle.sh" "$1"
EOF
cat >"${FIXTURE_DIR}/nested/scripts/lib/middle.sh" <<'EOF'
#!/usr/bin/env bash
bash "$(dirname "$0")/inner.sh" "$1"
EOF
cat >"${FIXTURE_DIR}/nested/scripts/lib/inner.sh" <<'EOF'
#!/usr/bin/env bash
terraform -chdir="$1" state rm module.networking.thing
EOF

assert_scan "a write two scripts deep is still a write" \
  'destroy.yml: job "destroy-gcp": runs scripts/outer.sh against the gcp Terraform state with no concurrency group' \
  "${FIXTURE_DIR}/nested/wf" "${FIXTURE_DIR}/nested/scripts"
