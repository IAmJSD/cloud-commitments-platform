#!/usr/bin/env bash
# test-init-backend.sh
#
# Offline test for init-backend.sh (#306): the aws CLI is stubbed via PATH and
# the script runs inside a throwaway copy of the AWS Terraform root, so nothing
# here touches a real account or the working tree.
#
# The script used to write terraform/environments/aws/<env>/backend.tf with
# key "<env>/terraform.tfstate" and tell the operator to apply from that new
# directory, while every deploy workflow inits the flat root with
# key "github-<env>/terraform.tfstate" (or "github-fargate-<env>/..."). The
# expected key is read from the deploy workflows themselves rather than written
# here, so the test fails if either side changes without the other.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SUT="${SCRIPT_DIR}/init-backend.sh"
WORKFLOW_DIR="${REPO_ROOT}/.github/workflows"
AWS_ROOT_REL="terraform/environments/aws"

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

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# Stub aws. Every call is logged; the bucket probe reports NoSuchBucket so the
# create path runs, and every mutating s3api call succeeds without output.
cat >"$WORK/bin/aws" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$AWS_LOG"
args=" $* "
case "$args" in
  *" sts get-caller-identity "*"--query Account"*) echo "stub-account" ;;
  *" sts get-caller-identity "*) echo '{"Account":"stub-account"}' ;;
  *" s3 ls "*) echo "An error occurred (NoSuchBucket) when calling the ListObjectsV2 operation" >&2 ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/aws"

# workflow_key FILE ENV
#
# Prints the S3 state key FILE's Terraform Init step builds for ENV, taken from
# its `printf '...key = "github-...%s..."...'` line. Fails (prints nothing) when
# the file builds no key or more than one distinct key format.
workflow_key() {
  local file="$1" env="$2" formats
  formats="$(grep -oE 'key = "github-[^"]*%s[^"]*/terraform\.tfstate"' "$file" | sort -u)"
  if [[ -z "$formats" || "$(printf '%s\n' "$formats" | wc -l | tr -d ' ')" != "1" ]]; then
    return 0
  fi
  formats="${formats#key = \"}"
  formats="${formats%\"}"
  # shellcheck disable=SC2059  # the workflow's own format string is the point
  printf "$formats" "$env"
}

# new_fixture NAME: a copy of the AWS root's backend.tf, which is all the
# script needs to recognize the repository root.
new_fixture() {
  local dir="$WORK/$1"
  mkdir -p "$dir/${AWS_ROOT_REL}"
  cp "${REPO_ROOT}/${AWS_ROOT_REL}/backend.tf" "$dir/${AWS_ROOT_REL}/backend.tf"
  echo "$dir"
}

# run_sut DIR ARGS...: runs the script from DIR with the stub first on PATH.
# Sets RC, OUT (combined output) and AWS_LOG.
run_sut() {
  local dir="$1"
  shift
  AWS_LOG="$WORK/aws-$(basename "$dir").log"
  : >"$AWS_LOG"
  set +e
  OUT="$(cd "$dir" && AWS_LOG="$AWS_LOG" PATH="$WORK/bin:$PATH" bash "$SUT" "$@" 2>&1)"
  RC=$?
  set -e
}

# backend_value FILE NAME: the value of a `name = value` line, quotes dropped.
backend_value() {
  sed -nE "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*\"?([^\"]*)\"?[[:space:]]*$/\1/p" "$1"
}

check() {
  local label="$1"
  shift
  if "$@"; then note_pass "$label"; else note_fail "$label"; fi
}

# same_nonempty A B: A is non-empty and equal to B.
same_nonempty() { [[ -n "$1" && "$1" == "$2" ]]; }

# has TEXT NEEDLE / lacks TEXT NEEDLE: fixed-string search in captured output.
has() { [[ "$1" == *"$2"* ]]; }
lacks() { [[ "$1" != *"$2"* ]]; }

# The platform flag the plan step should carry. Built from a variable rather
# than written out, because test-aws-tfstate-platform-key.sh sweeps scripts/
# and reads a literal platform assignment in any script as a state-writing job.
platform_var() { printf 'compute_platform=%s' "$1"; }

# --- The keys CI uses --------------------------------------------------------
LAMBDA_KEY="$(workflow_key "${WORKFLOW_DIR}/deploy-aws-lambda.yml" dev)"
FARGATE_KEY="$(workflow_key "${WORKFLOW_DIR}/deploy-aws-fargate.yml" prod)"
check "deploy-aws-lambda.yml builds one recognizable state key" test -n "$LAMBDA_KEY"
check "deploy-aws-fargate.yml builds one recognizable state key" test -n "$FARGATE_KEY"
echo "      CI keys: lambda/dev=${LAMBDA_KEY:-<none>} fargate/prod=${FARGATE_KEY:-<none>}"

# --- Default (Lambda), legacy positional arguments ---------------------------
FX="$(new_fixture lambda)"
run_sut "$FX" dev us-east-1
BACKEND="$FX/${AWS_ROOT_REL}/backends/dev.tfbackend"
check "lambda: script exits 0" test "$RC" -eq 0
check "lambda: writes ${AWS_ROOT_REL}/backends/dev.tfbackend" test -f "$BACKEND"
check "lambda: creates no per-environment root (${AWS_ROOT_REL}/dev)" test ! -e "$FX/${AWS_ROOT_REL}/dev"
if [[ -f "$BACKEND" ]]; then
  got_key="$(backend_value "$BACKEND" key)"
  check "lambda: key '${got_key}' is the key deploy-aws-lambda.yml uses" \
    same_nonempty "$LAMBDA_KEY" "$got_key"
  check "lambda: bucket is the one the script created" \
    test "$(backend_value "$BACKEND" bucket)" = "cudly-terraform-state-dev"
  check "lambda: region is the one requested" test "$(backend_value "$BACKEND" region)" = "us-east-1"
  check "lambda: S3-native locking is on" test "$(backend_value "$BACKEND" use_lockfile)" = "true"
  # A -backend-config file holds bare attributes; a `terraform {}` block in it
  # is rejected by terraform init.
  check "lambda: file is a -backend-config file, not a terraform block" \
    test -z "$(grep -vE '^[[:space:]]*(#.*)?$' "$BACKEND" | grep -vE '^[a-z_]+[[:space:]]*=[[:space:]]*[^[:space:]]' || true)"
fi
check "lambda: tells the operator to init the flat root with that file" \
  has "$OUT" "terraform init -backend-config=backends/dev.tfbackend"
check "lambda: plans with the platform it keyed the state for" \
  has "$OUT" "$(platform_var lambda)"
check "lambda: does not send the operator into a per-environment directory" \
  lacks "$OUT" "cd ${AWS_ROOT_REL}/dev"

# --- Fargate, flag arguments -------------------------------------------------
FX="$(new_fixture fargate)"
run_sut "$FX" --environment prod --region eu-west-1 --compute-platform fargate
BACKEND="$FX/${AWS_ROOT_REL}/backends/prod.tfbackend"
check "fargate: script exits 0" test "$RC" -eq 0
check "fargate: writes ${AWS_ROOT_REL}/backends/prod.tfbackend" test -f "$BACKEND"
if [[ -f "$BACKEND" ]]; then
  got_key="$(backend_value "$BACKEND" key)"
  check "fargate: key '${got_key}' is the key deploy-aws-fargate.yml uses" \
    same_nonempty "$FARGATE_KEY" "$got_key"
  check "fargate: region is the one requested" test "$(backend_value "$BACKEND" region)" = "eu-west-1"
fi
check "fargate: plans with the platform it keyed the state for" \
  has "$OUT" "$(platform_var fargate)"

# --- The generated file can never be committed -------------------------------
if git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  check "backends/<env>.tfbackend is gitignored in this repository" \
    git -C "$REPO_ROOT" check-ignore -q "${AWS_ROOT_REL}/backends/prod.tfbackend"
else
  note_fail "not a git checkout; cannot check that the generated file is gitignored"
fi

# --- Rejections happen before any AWS call -----------------------------------
FX="$(new_fixture badplatform)"
run_sut "$FX" --environment dev --region us-east-1 --compute-platform ecs
check "unknown --compute-platform: exits non-zero" test "$RC" -ne 0
check "unknown --compute-platform: makes no AWS call" test ! -s "$AWS_LOG"

FX="$WORK/notroot"
mkdir -p "$FX"
run_sut "$FX" dev us-east-1
check "outside the repository root: exits non-zero" test "$RC" -ne 0
check "outside the repository root: makes no AWS call" test ! -s "$AWS_LOG"
check "outside the repository root: creates no terraform/ tree" test ! -e "$FX/terraform"

echo ""
echo "${pass} passed, ${fail} failed"
[[ "$fail" -eq 0 ]]
