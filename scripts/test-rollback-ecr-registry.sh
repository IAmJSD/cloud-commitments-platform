#!/usr/bin/env bash
# test-rollback-ecr-registry.sh
#
# Offline test for the "Verify image exists" step of the two AWS rollback jobs
# in .github/workflows/rollback.yml (#142). The step's `run:` block is lifted
# out of the workflow and executed against a stubbed `aws` on PATH, so what is
# tested is the text that ships, not a copy of it.
#
# The validate job builds IMAGE_URI from the AWS_ACCOUNT_ID repository
# variable, and Terraform then deploys that URI with the role in
# AWS_ROLE_TO_ASSUME. Before #142 the verify step asked ECR with no
# --registry-id, so it checked the assumed role's registry, which equals
# AWS_ACCOUNT_ID's only by convention. When the two diverged, the step
# reported "Image exists in ECR" for a registry Terraform does not pull from.
#
# The stub models the real CLI's defaults: `describe-images` without
# --registry-id resolves against the caller's account, and
# `sts get-caller-identity` returns the caller's account. PATH is limited to
# the stub directory plus /usr/bin:/bin, so no real AWS CLI is reachable.
#
# Account IDs are assembled at runtime from adjacent literals so this file
# never holds a contiguous 12-digit token for git-secrets to flag.
#
# Set ROLLBACK_WORKFLOW to run the same cases against another copy of the
# workflow (e.g. the pre-fix one, to see the cases fail).
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKFLOW="${ROLLBACK_WORKFLOW:-${REPO_ROOT}/.github/workflows/rollback.yml}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin" "$WORK/home"

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

ACCT_URI="111111""111111"
ACCT_ROLE="222222""222222"
REGION="us-east-1"
REPO="cudly"
TAG="sha-abc1234"
JOBS="rollback-aws-lambda rollback-aws-fargate"

# --- Step extraction ---------------------------------------------------------
#
# extract_block MODE JOB STEP
#
# Prints the `run:` block (MODE=run) or the `env:` mapping (MODE=env) of the
# step named STEP inside the top-level job JOB, de-indented. Jobs are delimited
# by two-space-indent headers, steps by `      - ` items, and a block ends at
# the first non-blank line indented less than its content.
extract_block() {
  awk -v mode="$1" -v job="$2" -v step="$3" '
    /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ { injob = ($0 == "  " job ":"); instep = 0; blk = ""; next }
    !injob { next }
    /^      - / { instep = ($0 == "      - name: " step); blk = ""; next }
    !instep { next }
    /^        [A-Za-z_-]+:/ {
      blk = ""
      if ($0 ~ /^        run: [|][[:space:]]*$/) blk = "run"
      else if ($0 ~ /^        env:[[:space:]]*$/) blk = "env"
      next
    }
    blk == mode {
      if ($0 ~ /^[[:space:]]*$/) { print ""; next }
      if (substr($0, 1, 10) == "          ") { print substr($0, 11); next }
      blk = ""
    }
  ' "$WORKFLOW"
}

# --- The aws stub ------------------------------------------------------------
cat >"$WORK/bin/aws" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-} ${2:-}" in
  "sts get-caller-identity")
    if [ "${STUB_STS_FAIL:-0}" = 1 ]; then
      echo "Unable to locate credentials. You can configure credentials by running \"aws configure\"." >&2
      exit 253
    fi
    # Model the output shape: without `--query Account --output text` the
    # real CLI prints JSON, which must never compare equal to an account ID.
    if [[ " $* " == *" --query Account "* && " $* " == *" --output text "* ]]; then
      printf '%s\n' "$STUB_CALLER_ACCOUNT"
    else
      printf '{"UserId":"x","Account":"%s","Arn":"arn"}\n' "$STUB_CALLER_ACCOUNT"
    fi
    exit 0
    ;;
  "ecr describe-images")
    shift 2
    # The real CLI defaults the registry to the caller's own account.
    registry="$STUB_CALLER_ACCOUNT"
    repo=""
    tag=""
    region=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --registry-id) registry="$2"; shift 2 ;;
        --repository-name) repo="$2"; shift 2 ;;
        --image-ids) tag="${2#imageTag=}"; shift 2 ;;
        --region) region="$2"; shift 2 ;;
        *) echo "stub aws: unexpected describe-images argument: $1" >&2; exit 2 ;;
      esac
    done
    for img in ${STUB_IMAGES:-}; do
      if [ "$img" = "${registry}/${region}/${repo}:${tag}" ]; then
        echo '{"imageDetails":[{"imageTags":["'"$tag"'"]}]}'
        exit 0
      fi
    done
    echo "An error occurred (ImageNotFoundException) when calling the DescribeImages operation" >&2
    exit 254
    ;;
esac
echo "stub aws: unexpected command: $*" >&2
exit 2
STUB
chmod +x "$WORK/bin/aws"

# run_step JOB AWS_ACCOUNT_ID CALLER_ACCOUNT IMAGES STS_FAIL ECR_REPOSITORY
#
# Runs JOB's verify step the way the runner does (bash -e -o pipefail with the
# step's env), with only the stub reachable. Sets STEP_RC, STEP_OUT, STEP_LOG.
run_step() {
  local job="$1"
  local script="$WORK/step-${job}.sh"
  STEP_LOG="$WORK/aws-${job}.log"
  : >"$STEP_LOG"
  extract_block run "$job" "Verify image exists" >"$script"
  set +e
  STEP_OUT="$(env -i \
    PATH="$WORK/bin:/usr/bin:/bin" \
    HOME="$WORK/home" \
    AWS_CONFIG_FILE=/dev/null \
    AWS_SHARED_CREDENTIALS_FILE=/dev/null \
    STUB_LOG="$STEP_LOG" \
    STUB_CALLER_ACCOUNT="$3" \
    STUB_IMAGES="$4" \
    STUB_STS_FAIL="$5" \
    IMAGE_TAG="$TAG" \
    ECR_REPOSITORY="$6" \
    AWS_ACCOUNT_ID="$2" \
    AWS_REGION="$REGION" \
    bash --noprofile --norc -eo pipefail "$script" 2>&1)"
  STEP_RC=$?
  set -e
}

# expect LABEL WANT(pass|fail) TEXT
expect() {
  local label="$1" want="$2" text="$3"
  local ok=1
  if [[ "$want" == pass && "$STEP_RC" -ne 0 ]] || [[ "$want" == fail && "$STEP_RC" -eq 0 ]]; then
    ok=0
  fi
  if [[ -n "$text" && "$STEP_OUT" != *"$text"* ]]; then
    ok=0
  fi
  if [[ "$ok" -eq 1 ]]; then
    note_pass "$label"
  else
    note_fail "$label"
    local wanted="the step to ${want}"
    if [[ -n "$text" ]]; then
      wanted="${wanted} with output containing '${text}'"
    fi
    echo "      wanted ${wanted}; exit ${STEP_RC}, output:"
    printf '%s\n' "$STEP_OUT" | sed 's/^/        /'
  fi
}

# expect_log LABEL (has|lacks) TEXT
expect_log() {
  local label="$1" want="$2" text="$3"
  if { [[ "$want" == has ]] && grep -Fq -- "$text" "$STEP_LOG"; } ||
    { [[ "$want" == lacks ]] && ! grep -Fq -- "$text" "$STEP_LOG"; }; then
    note_pass "$label"
  else
    note_fail "$label"
    echo "      wanted the aws call log to ${want} '${text}', log:"
    sed 's/^/        /' "$STEP_LOG"
  fi
}

# --- The steps exist and read the same variables as IMAGE_URI ----------------
#
# Asserted first: an extractor that finds nothing would make every behavioral
# case below fail for the wrong reason, or a future rename pass unnoticed.
validate_env="$(extract_block env validate "Validate inputs")"
if [[ "$validate_env" == *"AWS_ACCOUNT_ID:"* ]]; then
  note_pass "validate job's Validate inputs step is found"
else
  note_fail "validate job's Validate inputs step env not found in ${WORKFLOW}"
fi

for job in $JOBS; do
  run_block="$(extract_block run "$job" "Verify image exists")"
  if [[ "$run_block" == *"aws ecr describe-images"* ]]; then
    note_pass "${job}: Verify image exists step is found"
  else
    note_fail "${job}: no Verify image exists step calling aws ecr describe-images in ${WORKFLOW}"
    continue
  fi

  # The verified registry must come from the same expressions IMAGE_URI is
  # built from, or the step can check one registry while deploying another.
  verify_env="$(extract_block env "$job" "Verify image exists")"
  for key in AWS_ACCOUNT_ID ECR_REPOSITORY AWS_REGION IMAGE_TAG; do
    want_line="$(printf '%s\n' "$validate_env" | grep -E "^${key}:" || true)"
    got_line="$(printf '%s\n' "$verify_env" | grep -E "^${key}:" || true)"
    if [[ -n "$want_line" && "$got_line" == "$want_line" ]]; then
      note_pass "${job}: ${key} comes from the same expression as IMAGE_URI"
    else
      note_fail "${job}: ${key} differs from the IMAGE_URI source"
      echo "      validate: ${want_line:-<missing>}"
      echo "      verify:   ${got_line:-<missing>}"
    fi
  done
done

# --- Behavior ----------------------------------------------------------------
for job in $JOBS; do
  # Positive case first: a step that always fails would pass every refusal.
  run_step "$job" "$ACCT_URI" "$ACCT_URI" "${ACCT_URI}/${REGION}/${REPO}:${TAG}" 0 "$REPO"
  expect "${job}: matching accounts with the image present pass" pass "Image exists in ECR"
  expect_log "${job}: describe-images is pinned to the IMAGE_URI registry" has \
    "ecr describe-images --registry-id ${ACCT_URI} --repository-name ${REPO}"

  # #142: the image exists only in the assumed role's registry. The pre-fix
  # step checked that registry and passed.
  run_step "$job" "$ACCT_URI" "$ACCT_ROLE" "${ACCT_ROLE}/${REGION}/${REPO}:${TAG}" 0 "$REPO"
  expect "${job}: AWS_ACCOUNT_ID diverging from the assumed role is refused" fail \
    "does not match the assumed role's account"

  # Image present in both registries: the verify alone would now be
  # self-consistent, but Terraform still applies with the other account's
  # role, so the divergence itself must fail the step.
  run_step "$job" "$ACCT_URI" "$ACCT_ROLE" \
    "${ACCT_URI}/${REGION}/${REPO}:${TAG} ${ACCT_ROLE}/${REGION}/${REPO}:${TAG}" 0 "$REPO"
  expect "${job}: divergence is refused even when both registries hold the tag" fail \
    "does not match the assumed role's account"
  expect_log "${job}: no image lookup runs once the accounts disagree" lacks "ecr describe-images"

  run_step "$job" "" "$ACCT_ROLE" "${ACCT_ROLE}/${REGION}/${REPO}:${TAG}" 0 "$REPO"
  expect "${job}: an unset AWS_ACCOUNT_ID is refused" fail "AWS_ACCOUNT_ID repository variable is not set"

  run_step "$job" "$ACCT_URI" "$ACCT_URI" "${ACCT_URI}/${REGION}/${REPO}:${TAG}" 1 "$REPO"
  expect "${job}: an unreadable caller identity is refused" fail \
    "Could not determine the assumed role's AWS account"
  expect_log "${job}: no image lookup runs without a caller identity" lacks "ecr describe-images"

  run_step "$job" "$ACCT_URI" "$ACCT_URI" "${ACCT_URI}/${REGION}/${REPO}:sha-other" 0 "$REPO"
  expect "${job}: a missing tag in the IMAGE_URI registry is refused" fail "Image not found in ECR"

  run_step "$job" "$ACCT_URI" "$ACCT_URI" "${ACCT_URI}/${REGION}/${REPO}:${TAG}" 0 ""
  expect "${job}: an unset ECR_REPOSITORY is refused" fail "ECR_REPOSITORY repository variable is not set"
done

echo
echo "passed: ${pass}, failed: ${fail}"
[[ "$fail" -eq 0 ]]
