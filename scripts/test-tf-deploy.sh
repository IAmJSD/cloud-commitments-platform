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
# script's profile check, environment-directory bootstrap and `terraform init`.
# It now calls the script with `-target=module.frontend`.
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
cat > "$WORK/bin/terraform" <<'STUB'
#!/usr/bin/env bash
line=""
for a in "$@"; do line="${line}|${a}"; done
printf '%s\n' "${line#|}" >> "$TF_STUB_LOG"
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

# new_sandbox NAME: fresh project root with the files under test and an
# aws/dev profile. Prints the root.
new_sandbox() {
  local root="$WORK/$1"
  mkdir -p "$root/scripts" "$root/terraform/profiles/aws" "$root/terraform/environments/aws"
  cp "$MAKEFILE_SRC" "$root/Makefile.terraform"
  cp "$SUT_SRC" "$root/scripts/tf-deploy.sh"
  chmod +x "$root/scripts/tf-deploy.sh"
  : > "$root/terraform/profiles/aws/dev.tfvars"
  : > "$root/terraform/environments/aws/main.tf"
  : > "$root/stub.log"
  printf '%s\n' "$root"
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

# --- 2. fresh checkout: env dir bootstrapped, init run, apply targeted --------
root="$(new_sandbox fresh)"
if TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1; then
  ok "make frontend-only succeeds on a fresh tree"
else
  ko "make frontend-only failed on a fresh tree"; sed 's/^/    /' "$root/out"
fi
expect_log "make frontend-only on a fresh tree runs init, the targeted apply, then output" "$root" \
"init
apply|-var-file=$root/terraform/profiles/aws/dev.tfvars|-target=module.frontend
output"
if [ -L "$root/terraform/environments/aws/dev/main.tf" ] &&
   [ "$(readlink "$root/terraform/environments/aws/dev/main.tf")" = "../../aws/main.tf" ]; then
  ok "make frontend-only bootstraps the environment directory and main.tf symlink"
else
  ko "make frontend-only did not bootstrap terraform/environments/aws/dev"
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

# --- 3. env dir exists but was never initialised: init still runs -------------
root="$(new_sandbox existing-env)"
mkdir -p "$root/terraform/environments/aws/dev"
TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1 || true
expect_log "make frontend-only in an uninitialised env dir runs init first" "$root" \
"init
apply|-var-file=$root/terraform/profiles/aws/dev.tfvars|-target=module.frontend
output"

# --- 4. initialised env dir: no init, targeted apply, output -------------------
root="$(new_sandbox initialised)"
mkdir -p "$root/terraform/environments/aws/dev/.terraform"
TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=dev > "$root/out" 2>&1 || true
expect_log "make frontend-only in an initialised env dir skips init" "$root" \
"apply|-var-file=$root/terraform/profiles/aws/dev.tfvars|-target=module.frontend
output"

# --- 5. missing profile: fails before terraform is ever called -----------------
root="$(new_sandbox missing-profile)"
# The env dir exists, so only the profile check can stop the apply.
mkdir -p "$root/terraform/environments/aws/nope/.terraform"
if TF_STUB_LOG="$root/stub.log" make -s -C "$root" -f Makefile.terraform frontend-only PROVIDER=aws PROFILE=nope > "$root/out" 2>&1; then
  ko "make frontend-only with a missing profile exited 0"
else
  ok "make frontend-only with a missing profile exits non-zero"
fi
expect_log "make frontend-only with a missing profile never calls terraform" "$root" ""
if grep -q 'Profile not found' "$root/out"; then
  ok "make frontend-only with a missing profile names the missing file"
else
  ko "make frontend-only with a missing profile gave no 'Profile not found' error"; sed 's/^/    /' "$root/out"
fi

# --- 6. the script forwards extra args verbatim, and adds none when absent -----
root="$(new_sandbox forwarding)"
mkdir -p "$root/terraform/environments/aws/dev/.terraform"
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
