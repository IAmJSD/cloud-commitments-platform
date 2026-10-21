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
