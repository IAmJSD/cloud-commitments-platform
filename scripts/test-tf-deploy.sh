#!/usr/bin/env bash
# test-tf-deploy.sh
#
# Offline test for scripts/tf-deploy.sh and the Makefile.terraform targets that
# drive it. `terraform` is a PATH stub that records its argv and does nothing
# else, so nothing here plans, applies or talks to a backend.
#
# Each case runs in a throwaway copy of the two files under test, laid out like
# the repo (Makefile.terraform at the root, the script under scripts/, profiles
# under terraform/profiles/), so the real tree is never touched.
#
# The headline case is #59: `make frontend-only` used to cd into the
# environment directory and run `terraform apply` itself, which skipped the
# script's profile check and `terraform init`. It now calls the script with
# `-target=module.frontend`.
#
# Issue #349: the script used to build terraform/environments/<provider>/<profile>,
# a directory this repo does not have, symlink only main.tf into it and run
# `terraform init` with no backend config. It now runs in the flat root
# terraform/environments/<provider>, inits with
# -backend-config=backends/<profile>.tfbackend, and gives each profile its own
# TF_DATA_DIR so two profiles never share a backend.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MAKEFILE_SRC="${REPO_ROOT}/Makefile.terraform"
SUT_SRC="${SCRIPT_DIR}/tf-deploy.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# Stub terraform: one line per call, arguments joined by '|' so an argument
# that is empty or contains a space stays visible. `init` and `output` are
# recorded too, so the log shows the script's bootstrap and summary steps.
# Where each call ran (working directory and TF_DATA_DIR) goes to a second
# log, $TF_STUB_LOG.env. `init` creates TF_DATA_DIR, as the real one does, so
# a second run in the same profile skips init.
cat > "$WORK/bin/terraform" <<'STUB'
#!/usr/bin/env bash
line=""
for a in "$@"; do line="${line}|${a}"; done
printf '%s\n' "${line#|}" >> "$TF_STUB_LOG"
printf '%s|%s\n' "$PWD" "${TF_DATA_DIR:-}" >> "$TF_STUB_LOG.env"
if [ "${1:-}" = "init" ] && [ -n "${TF_DATA_DIR:-}" ]; then mkdir -p "$TF_DATA_DIR"; fi
STUB
chmod +x "$WORK/bin/terraform"

export PATH="$WORK/bin:$PATH"
# Belt and braces: no case should ever reach a real cloud credential.
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_EC2_METADATA_DISABLED=true
unset AWS_PROFILE TF_VAR_aws_profile

pass=0
fail=0

ok() { echo "PASS: $1"; pass=$((pass + 1)); }
ko() { echo "FAIL: $1"; fail=$((fail + 1)); }

# new_sandbox NAME: fresh project root with the files under test, the flat AWS
# root (main.tf, backend.tf) and an aws/dev profile with its backend config.
# Prints the root.
new_sandbox() {
  local root="$WORK/$1"
  mkdir -p "$root/scripts" "$root/terraform/profiles/aws" "$root/terraform/environments/aws/backends"
  cp "$MAKEFILE_SRC" "$root/Makefile.terraform"
  cp "$SUT_SRC" "$root/scripts/tf-deploy.sh"
  chmod +x "$root/scripts/tf-deploy.sh"
  : > "$root/terraform/profiles/aws/dev.tfvars"
  : > "$root/terraform/environments/aws/main.tf"
  : > "$root/terraform/environments/aws/backend.tf"
  : > "$root/terraform/environments/aws/backends/dev.tfbackend"
  : > "$root/stub.log"
  : > "$root/stub.log.env"
  printf '%s\n' "$root"
}

# expect_env LABEL ROOT EXPECTED: the stub's cwd|TF_DATA_DIR log must equal
# EXPECTED exactly.
expect_env() {
  local label="$1" root="$2" expected="$3" actual
  actual="$(cat "$root/stub.log.env")"
  if [ "$actual" = "$expected" ]; then
    ok "$label"
  else
    ko "$label"
    echo "  expected cwd|TF_DATA_DIR:"; printf '%s\n' "$expected" | sed 's/^/    /'
    echo "  actual cwd|TF_DATA_DIR:";   printf '%s\n' "$actual"   | sed 's/^/    /'
  fi
}

# expect_log LABEL ROOT EXPECTED: the stub log must equal EXPECTED exactly.
expect_log() {
  local label="$1" root="$2" expected="$3" actual
  actual="$(cat "$root/stub.log")"
  if [ "$actual" = "$expected" ]; then
    ok "$label"
  else
    ko "$label"
    echo "  expected terraform calls:"; printf '%s\n' "$expected" | sed 's/^/    /'
    echo "  actual terraform calls:";   printf '%s\n' "$actual"   | sed 's/^/    /'
  fi
}

# --- 1. make -n prints the script invocation, not a raw terraform apply -------
root="$(new_sandbox dry-run)"
dry="$(make -s -n -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev 2>&1)"
if printf '%s\n' "$dry" | grep -qxF './scripts/tf-deploy.sh aws dev apply -target=module.frontend'; then
  ok "make -n frontend-only prints ./scripts/tf-deploy.sh aws dev apply -target=module.frontend"
else
  ko "make -n frontend-only does not route through tf-deploy.sh"; printf '%s\n' "$dry" | sed 's/^/    /'
fi
if printf '%s\n' "$dry" | grep -Eq '(^|[^-[:alnum:]])terraform[[:space:]]+apply'; then
  ko "make -n frontend-only still runs terraform apply directly"
else
  ok "make -n frontend-only has no direct terraform apply"
fi

# --- 2. fresh checkout: flat root, backend config, targeted apply --------------
root="$(new_sandbox fresh)"
aws="$root/terraform/environments/aws"
if TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1; then
  ok "make frontend-only succeeds on a fresh tree"
else
  ko "make frontend-only failed on a fresh tree"; sed 's/^/    /' "$root/out"
fi
expect_log "make frontend-only on a fresh tree inits with the profile's backend config, applies, then outputs" "$root" \
"init|-backend-config=$aws/backends/dev.tfbackend
apply|-var-file=$root/terraform/profiles/aws/dev.tfvars|-target=module.frontend
output"
expect_env "every call runs in the flat root with the profile's own TF_DATA_DIR" "$root" \
"$aws|$aws/.terraform/profiles/dev
$aws|$aws/.terraform/profiles/dev
$aws|$aws/.terraform/profiles/dev"
if [ -e "$aws/dev" ]; then
  ko "tf-deploy.sh still creates the per-profile directory terraform/environments/aws/dev"
else
  ok "tf-deploy.sh creates no per-profile directory under terraform/environments/aws"
fi
if grep -q 'Deployment Outputs' "$root/out"; then
  ok "make frontend-only prints the script's Deployment Outputs summary"
else
  ko "make frontend-only skipped the Deployment Outputs summary"; sed 's/^/    /' "$root/out"
fi
if grep -q 'AWS_PROFILE not set' "$root/out"; then
  ok "make frontend-only prints the AWS_PROFILE warning"
else
  ko "make frontend-only skipped the AWS_PROFILE warning"
fi

# --- 3. a second profile gets its own init, backend and data dir ---------------
# With one shared .terraform, prod would run against the dev backend's state.
root="$(new_sandbox two-profiles)"
aws="$root/terraform/environments/aws"
: > "$root/terraform/profiles/aws/prod.tfvars"
: > "$aws/backends/prod.tfbackend"
TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" aws dev plan > "$root/out" 2>&1 || true
TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" aws prod plan > "$root/out" 2>&1 || true
TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" aws dev plan > "$root/out" 2>&1 || true
expect_log "dev then prod then dev: prod inits with its own backend, dev is not re-initialised" "$root" \
"init|-backend-config=$aws/backends/dev.tfbackend
plan|-var-file=$root/terraform/profiles/aws/dev.tfvars
init|-backend-config=$aws/backends/prod.tfbackend
plan|-var-file=$root/terraform/profiles/aws/prod.tfvars
plan|-var-file=$root/terraform/profiles/aws/dev.tfvars"
expect_env "dev and prod each run with their own TF_DATA_DIR" "$root" \
"$aws|$aws/.terraform/profiles/dev
$aws|$aws/.terraform/profiles/dev
$aws|$aws/.terraform/profiles/prod
$aws|$aws/.terraform/profiles/prod
$aws|$aws/.terraform/profiles/dev"

# --- 4. initialised profile: no init, targeted apply, output -------------------
root="$(new_sandbox initialised)"
mkdir -p "$root/terraform/environments/aws/.terraform/profiles/dev"
TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1 || true
expect_log "make frontend-only with an initialised profile skips init" "$root" \
"apply|-var-file=$root/terraform/profiles/aws/dev.tfvars|-target=module.frontend
output"

# --- 5. missing inputs and bad names fail before terraform is ever called ------
# refuse_case NAME LABEL MESSAGE ARGS...: tf-deploy.sh must exit non-zero, call
# no terraform, and print MESSAGE. $SETUP, if set, runs in the sandbox first.
refuse_case() {
  local name="$1" label="$2" message="$3" root
  shift 3
  root="$(new_sandbox "$name")"
  # An initialised data dir, so only the check under test can stop the run.
  mkdir -p "$root/terraform/environments/aws/.terraform/profiles/dev"
  if [ -n "${SETUP:-}" ]; then (cd "$root" && eval "$SETUP"); fi
  if TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" "$@" > "$root/out" 2>&1; then
    ko "$label exited 0"
  else
    ok "$label exits non-zero"
  fi
  expect_log "$label never calls terraform" "$root" ""
  if grep -qF "$message" "$root/out"; then
    ok "$label says '$message'"
  else
    ko "$label did not say '$message'"; sed 's/^/    /' "$root/out"
  fi
}
refuse_case missing-profile "a missing profile" "Profile not found" aws nope apply
SETUP='rm terraform/environments/aws/backends/dev.tfbackend' \
  refuse_case missing-backend "a profile with no backend config" "Backend config not found" aws dev apply
root="$(new_sandbox missing-backend-make)"
rm "$root/terraform/environments/aws/backends/dev.tfbackend"
if TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1; then
  ko "make frontend-only with no backend config exited 0"
else
  ok "make frontend-only with no backend config exits non-zero"
fi
expect_log "make frontend-only with no backend config never calls terraform" "$root" ""
refuse_case unknown-provider "an unknown provider" "Unknown provider" oci dev apply
refuse_case traversal-provider "a provider that walks out of terraform/environments" "Unknown provider" ../aws dev apply
refuse_case traversal-profile "a profile that walks out of terraform/profiles" "Invalid profile name" aws ../aws/dev apply
SETUP='mkdir -p terraform/profiles/gcp && : > terraform/profiles/gcp/dev.tfvars' \
  refuse_case no-root "a provider whose root is missing" "Terraform root not found" gcp dev apply

# --- 6. the script forwards extra args verbatim, and adds none when absent -----
root="$(new_sandbox forwarding)"
mkdir -p "$root/terraform/environments/aws/.terraform/profiles/dev"
TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" aws dev plan > "$root/out" 2>&1 || true
TF_STUB_LOG="$root/stub.log" "$root/scripts/tf-deploy.sh" aws dev plan '-var=note=a b' -target=module.frontend > "$root/out" 2>&1 || true
expect_log "tf-deploy.sh adds no empty argument without extras and forwards extras verbatim" "$root" \
"plan|-var-file=$root/terraform/profiles/aws/dev.tfvars
plan|-var-file=$root/terraform/profiles/aws/dev.tfvars|-var=note=a b|-target=module.frontend"

# --- 7. no Makefile.terraform recipe runs a state-changing terraform command ---
# directly. apply/plan/destroy/refresh/import must go through tf-deploy.sh so
# they all get the same profile check and init. Recipe lines start with a tab.
direct="$(grep -nE $'^\t.*(^|[^-[:alnum:]])terraform[[:space:]]+(apply|plan|destroy|refresh|import)([[:space:]]|$)' "$MAKEFILE_SRC" || true)"
if [ -z "$direct" ]; then
  ok "no Makefile.terraform recipe runs terraform apply/plan/destroy/refresh/import directly"
else
  ko "Makefile.terraform recipes bypass tf-deploy.sh:"; printf '%s\n' "$direct" | sed 's/^/    /'
fi

echo ""
echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
