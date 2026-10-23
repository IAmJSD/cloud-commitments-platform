#!/usr/bin/env bash
# test-gcp-import-dev-state.sh
#
# Asserts that scripts/gcp-import-dev-state.sh takes the GCP project and the
# Terraform state backend from GCP_PROJECT_ID and TF_BACKEND_GCP, with no
# built-in default, and refuses to start without them (#305).
#
# The script used to carry the dev project ID and the state bucket as
# unconditional literals, so pointing it at any other environment silently
# imported the dev project's resources into whatever state was initialised.
#
# Every case runs the real script end to end against stubbed `terraform` and
# `gcloud` on PATH, in a clean environment (env -i), so nothing here reaches
# GCP or a real state backend. The stubs log every call they receive.
#
# Two halves, positive first:
#   1. With both variables set, every gcloud call targets exactly the given
#      project, every terraform import ID names it, terraform sees it as
#      TF_VAR_project_id, and the backend file passed to `terraform init` is
#      exactly TF_BACKEND_GCP plus the github-dev prefix. The calls are counted
#      first, since a run that makes no calls has no wrong project either.
#   2. With either variable missing or malformed, the script exits 2 with a
#      usage message BEFORE it calls terraform or gcloud or writes a backend
#      file.
#
# Half 1 needs bash >= 4, because the script under test uses `declare -A`
# (the bash 3.2 abort is tracked separately). The script under test runs under
# the same bash as this test ("$BASH"), not whichever `bash` comes first on the
# stub PATH, so the version check below is about the bash that actually runs
# it. On an older bash those cases are reported as SKIP locally and as FAIL
# under CI, so CI can never pass them by not running them.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${SCRIPT_DIR}/gcp-import-dev-state.sh"

PROJECT_A="example-dev-project"
PROJECT_B="other-sandbox-42"
BACKEND_A='bucket = "example-tfstate-bucket"'

pass=0
fail=0

ok() {
  echo "PASS: $1"
  pass=$((pass + 1))
}

ko() {
  echo "FAIL: $1"
  shift
  local line
  for line in "$@"; do
    echo "      $line"
  done
  fail=$((fail + 1))
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STUB_DIR="${WORK}/bin"
CALLS="${WORK}/calls.log"
CAPTURED_BACKEND="${WORK}/backend.captured"
RUN_TMP="${WORK}/tmp"
CREDS="${WORK}/creds.json"
mkdir -p "$STUB_DIR" "$RUN_TMP"
printf '{}\n' >"$CREDS"

# terraform stub: logs each call with the TF_VAR_project_id it saw. On init it
# copies the -backend-config file aside and records whether its mode was 0600.
# `state show` fails so every discovered resource is imported.
cat >"${STUB_DIR}/terraform" <<'STUB'
#!/usr/bin/env bash
printf 'terraform TF_VAR_project_id=%s %s\n' "${TF_VAR_project_id:-<unset>}" "$*" >>"$STUB_CALLS"
case "${1:-}" in
  init)
    for arg in "$@"; do
      case "$arg" in
        -backend-config=*)
          f="${arg#-backend-config=}"
          cat "$f" >"$STUB_CAPTURED_BACKEND"
          if [ -n "$(find "$f" -perm 0600)" ]; then
            echo "backend-mode=0600" >>"$STUB_CALLS"
          else
            echo "backend-mode=other" >>"$STUB_CALLS"
          fi
          ;;
      esac
    done
    echo "Terraform has been successfully initialized!"
    ;;
  state)
    [ "${2:-}" = "show" ] && exit 1
    ;;
esac
exit 0
STUB

# gcloud stub: logs each call and reports every resource as existing, so the
# script exercises every import branch.
cat >"${STUB_DIR}/gcloud" <<'STUB'
#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >>"$STUB_CALLS"
case "$*" in
  "secrets versions list"*) echo "projects/stub/secrets/stub/versions/7" ;;
  "sql users list"*) echo "cudly" ;;
  "services vpc-peerings list"*) echo "servicenetworking.googleapis.com" ;;
  *) echo "stub-resource" ;;
esac
exit 0
STUB
chmod +x "${STUB_DIR}/terraform" "${STUB_DIR}/gcloud"

RUN_EXIT=0
RUN_ERR=""

# run_target [VAR=value ...]: runs the script in a clean environment with the
# stubs first on PATH. Only the given variables (plus PATH, HOME, TMPDIR and
# credentials) are set, so nothing leaks in from the caller's shell.
run_target() {
  : >"$CALLS"
  rm -f "$CAPTURED_BACKEND"
  RUN_EXIT=0
  env -i \
    PATH="${STUB_DIR}:/usr/bin:/bin:/usr/sbin:/sbin" \
    HOME="$WORK" \
    TMPDIR="$RUN_TMP" \
    GOOGLE_APPLICATION_CREDENTIALS="$CREDS" \
    STUB_CALLS="$CALLS" \
    STUB_CAPTURED_BACKEND="$CAPTURED_BACKEND" \
    "$@" \
    "$BASH" "$TARGET" >"${WORK}/out" 2>"${WORK}/err" || RUN_EXIT=$?
  RUN_ERR="$(cat "${WORK}/err")"
}

leftover_backend_files() {
  find "$RUN_TMP" -name 'gcp-dev-backend*' | wc -l | tr -d ' '
}

# ---------------------------------------------------------------------------
# Half 1: the configured project and backend are the ones used
# ---------------------------------------------------------------------------

# assert_targets LABEL PROJECT BACKEND [extra VAR=value ...]
assert_targets() {
  local label="$1" project="$2" backend="$3"
  shift 3
  run_target GCP_PROJECT_ID="$project" TF_BACKEND_GCP="$backend" "$@"

  if [ "$RUN_EXIT" -ne 0 ]; then
    ko "$label: script exits 0" "exit $RUN_EXIT" "stderr: $RUN_ERR"
    return
  fi
  ok "$label: script exits 0"

  local gcloud_calls import_calls
  gcloud_calls="$(grep -c '^gcloud ' "$CALLS" || true)"
  import_calls="$(grep -c '^terraform .* import ' "$CALLS" || true)"
  if [ "$gcloud_calls" -ge 15 ] && [ "$import_calls" -ge 20 ]; then
    ok "$label: queried GCP and imported ($gcloud_calls gcloud calls, $import_calls imports)"
  else
    ko "$label: queried GCP and imported" "gcloud calls: $gcloud_calls, imports: $import_calls"
  fi

  local bad
  bad="$(grep '^gcloud ' "$CALLS" | grep -Ev -- "--project=${project}( |$)" || true)"
  if [ -z "$bad" ]; then
    ok "$label: every gcloud call passes --project=${project}"
  else
    ko "$label: every gcloud call passes --project=${project}" "offending: $bad"
  fi

  local projects_named
  projects_named="$(grep -o -- '--project=[^ ]*' "$CALLS" | sort -u | tr '\n' ' ')"
  if [ "$projects_named" = "--project=${project} " ]; then
    ok "$label: no gcloud call names another project"
  else
    ko "$label: no gcloud call names another project" "seen: $projects_named"
  fi

  # Check the import ID alone: the stub's log prefix carries TF_VAR_project_id,
  # so matching the whole line would always find the project. Strip
  # "terraform TF_VAR_project_id=... import <address> " (addresses have no
  # spaces; IAM member IDs do). Every ID must lead with the project, either as
  # projects/<project>/... or bare (<project>/region/... for the router and
  # NAT, "<project> roles/..." for project IAM members).
  local ids id_projects sa_projects
  ids="$(grep '^terraform .* import ' "$CALLS" |
    sed -E 's/^terraform TF_VAR_project_id=[^ ]* import [^ ]+ //' || true)"
  bad="$(printf '%s\n' "$ids" | grep -Ev -- "^(projects/)?${project}[/ ]" || true)"
  id_projects="$(printf '%s\n' "$ids" | grep -o 'projects/[^/ ]*' | sort -u | tr '\n' ' ')"
  sa_projects="$(printf '%s\n' "$ids" | grep -o '@[^ ]*\.iam\.gserviceaccount\.com' | sort -u | tr '\n' ' ')"
  if [ -z "$bad" ] && [ "$id_projects" = "projects/${project} " ] &&
    [ "$sa_projects" = "@${project}.iam.gserviceaccount.com " ]; then
    ok "$label: every import ID names ${project} and no other project"
  else
    ko "$label: every import ID names ${project} and no other project" \
      "IDs not led by it: $bad" "projects/ prefixes seen: $id_projects" \
      "service account projects seen: $sa_projects"
  fi

  bad="$(grep '^terraform ' "$CALLS" | grep -v "^terraform TF_VAR_project_id=${project} " || true)"
  if [ -z "$bad" ]; then
    ok "$label: terraform sees TF_VAR_project_id=${project}"
  else
    ko "$label: terraform sees TF_VAR_project_id=${project}" "offending: $bad"
  fi

  local want got
  want="$(printf '%s\nprefix = "github-dev"' "$backend")"
  got="$(cat "$CAPTURED_BACKEND" 2>/dev/null || echo '<no backend file captured>')"
  if [ "$got" = "$want" ]; then
    ok "$label: terraform init gets TF_BACKEND_GCP plus the github-dev prefix"
  else
    ko "$label: terraform init gets TF_BACKEND_GCP plus the github-dev prefix" \
      "expected: $want" "actual:   $got"
  fi

  if grep -qx 'backend-mode=0600' "$CALLS"; then
    ok "$label: backend file is mode 0600"
  else
    ko "$label: backend file is mode 0600" "$(grep '^backend-mode=' "$CALLS" || echo 'init not called')"
  fi

  if [ "$(leftover_backend_files)" = "0" ]; then
    ok "$label: backend file removed on exit"
  else
    ko "$label: backend file removed on exit" "$(find "$RUN_TMP" -name 'gcp-dev-backend*')"
  fi
}

if [ "${BASH_VERSINFO[0]}" -ge 4 ]; then
  assert_targets "project A" "$PROJECT_A" "$BACKEND_A"
  # A second, unrelated project and a multi-line backend, so a hardcoded value
  # cannot pass by coinciding with the first fixture.
  assert_targets "project B" "$PROJECT_B" "$(printf 'bucket = "other-state"\nimpersonate_service_account = "tf@%s.iam.gserviceaccount.com"' "$PROJECT_B")"
  # A TF_VAR_project_id that agrees with GCP_PROJECT_ID is accepted.
  assert_targets "matching TF_VAR_project_id" "$PROJECT_A" "$BACKEND_A" TF_VAR_project_id="$PROJECT_A"
elif [ "${GITHUB_ACTIONS:-}" = "true" ] || [ "${CI:-}" = "true" ]; then
  ko "end-to-end cases need bash >= 4 (running ${BASH_VERSION})"
else
  echo "SKIP: end-to-end cases need bash >= 4 (running ${BASH_VERSION}); CI runs them"
fi

# ---------------------------------------------------------------------------
# Half 2: missing or malformed configuration is refused before any side effect
# ---------------------------------------------------------------------------

# assert_refused LABEL EXPECTED_STDERR_SUBSTRING [VAR=value ...]
assert_refused() {
  local label="$1" want_err="$2"
  shift 2
  run_target "$@"

  local ncalls
  ncalls="$(grep -c . "$CALLS" || true)"
  if [ "$RUN_EXIT" -eq 2 ] &&
    [ "$ncalls" -eq 0 ] &&
    [ "$(leftover_backend_files)" = "0" ] &&
    printf '%s' "$RUN_ERR" | grep -qF -- "$want_err" &&
    printf '%s' "$RUN_ERR" | grep -qF -- "usage:"; then
    ok "refused: $label"
  else
    ko "refused: $label" \
      "expected exit 2, got $RUN_EXIT" \
      "expected 0 terraform/gcloud calls, got $ncalls" \
      "backend files left in TMPDIR: $(leftover_backend_files)" \
      "expected stderr to contain '$want_err' and 'usage:'" \
      "stderr: $RUN_ERR"
  fi
}

NL='
'

assert_refused "neither variable set" "GCP_PROJECT_ID is not set"
assert_refused "GCP_PROJECT_ID unset" "GCP_PROJECT_ID is not set" \
  TF_BACKEND_GCP="$BACKEND_A"
assert_refused "GCP_PROJECT_ID empty" "GCP_PROJECT_ID is not set" \
  GCP_PROJECT_ID="" TF_BACKEND_GCP="$BACKEND_A"
assert_refused "TF_BACKEND_GCP unset" "TF_BACKEND_GCP is not set" \
  GCP_PROJECT_ID="$PROJECT_A"
assert_refused "TF_BACKEND_GCP empty" "TF_BACKEND_GCP is not set" \
  GCP_PROJECT_ID="$PROJECT_A" TF_BACKEND_GCP=""
assert_refused "GCP_PROJECT_ID with uppercase" "GCP_PROJECT_ID is malformed" \
  GCP_PROJECT_ID="Example-Dev-Project" TF_BACKEND_GCP="$BACKEND_A"
assert_refused "GCP_PROJECT_ID too short" "GCP_PROJECT_ID is malformed" \
  GCP_PROJECT_ID="abc" TF_BACKEND_GCP="$BACKEND_A"
assert_refused "GCP_PROJECT_ID whitespace only" "GCP_PROJECT_ID is malformed" \
  GCP_PROJECT_ID="   " TF_BACKEND_GCP="$BACKEND_A"
assert_refused "GCP_PROJECT_ID with an embedded newline" "GCP_PROJECT_ID is malformed" \
  GCP_PROJECT_ID="${PROJECT_A}${NL}${PROJECT_B}" TF_BACKEND_GCP="$BACKEND_A"
assert_refused "TF_BACKEND_GCP is a bare bucket name" "has no bucket" \
  GCP_PROJECT_ID="$PROJECT_A" TF_BACKEND_GCP="example-tfstate-bucket"
assert_refused "TF_BACKEND_GCP sets its own prefix" "must not set prefix" \
  GCP_PROJECT_ID="$PROJECT_A" TF_BACKEND_GCP="${BACKEND_A}${NL}prefix = \"github-prod\""
assert_refused "TF_VAR_project_id names a different project" "does not match GCP_PROJECT_ID" \
  GCP_PROJECT_ID="$PROJECT_A" TF_BACKEND_GCP="$BACKEND_A" TF_VAR_project_id="$PROJECT_B"

echo
echo "Results: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
