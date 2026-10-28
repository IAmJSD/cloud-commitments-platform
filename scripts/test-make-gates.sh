#!/usr/bin/env bash
# test-make-gates.sh
#
# Asserts that the Makefile's local gates fail when they should (issue #349):
#
#   1. lint, security-scan-docker, security-scan-terraform and
#      security-scan-snyk exit non-zero when their tool is missing, and when
#      the tool itself exits non-zero. Each used to end its `then` branch with
#      a success echo, so the target passed either way.
#   2. security-scan no longer runs the deprecated tfsec target.
#   3. terraform-validate runs `terraform init` + `validate` in every flat
#      terraform/environments/<cloud> root and fails when one fails. It used to
#      loop over terraform/environments/*/dev, which does not exist.
#   4. Makefile.terraform's clean-locks refuses to delete the provider lock
#      files without CONFIRM_CLEAN_LOCKS=yes.
#   5. The Go targets' ./... (vet, test-unit, test-compile-integration,
#      test-integration, test-coverage, fmt) skips frontend/node_modules
#      (issue #177). Go's ./... used to pick up the Go source npm packages
#      ship there (frontend/node_modules/flatted/golang) once deps were
#      installed. Checked with the real go command and an -overlay probe, so
#      no npm install is needed and nothing is written into the repo.
#
# Tools are stubs on a PATH that holds nothing else, so the real tools are
# never run and nothing leaves the machine. Exits 0 when all cases pass;
# exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MAKE_BIN="$(command -v make)"

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

# stub NAME EXIT_CODE: a PATH dir whose only command is NAME, logging its argv
# and working directory to $WORK/<NAME>.log and exiting EXIT_CODE.
stub() {
  local dir="$WORK/bin-$1-$2"
  mkdir -p "$dir"
  cat > "$dir/$1" <<EOF
#!/bin/sh
echo "\$PWD \$*" >> "$WORK/$1.log"
exit $2
EOF
  chmod +x "$dir/$1"
  rm -f "$WORK/$1.log"
  echo "$dir"
}

# run_make PATH_VALUE ARGS...: run make in the repo with PATH set to exactly
# PATH_VALUE. make and its /bin/sh are absolute paths, and the recipes under
# test use only shell builtins besides the tool, so nothing else is needed.
run_make() {
  local path="$1"
  shift
  PATH="$path" "$MAKE_BIN" --no-print-directory -C "$REPO_ROOT" "$@" > "$WORK/out.log" 2>&1
}

expect_status() {
  local want="$1" desc="$2"
  shift 2
  local got=0
  "$@" || got=$?
  if { [ "$want" = 0 ] && [ "$got" = 0 ]; } || { [ "$want" != 0 ] && [ "$got" != 0 ]; }; then
    note_pass "$desc"
  else
    note_fail "$desc (want ${want/1/non-zero}, got $got)"
    sed 's/^/    /' "$WORK/out.log" 2>/dev/null || true
  fi
}

mkdir -p "$WORK/empty"

# 1. Scanner targets.
for pair in lint:golangci-lint security-scan-docker:trivy security-scan-terraform:tfsec security-scan-snyk:snyk; do
  target="${pair%%:*}"
  tool="${pair#*:}"
  expect_status 1 "$target fails when $tool is missing" \
    run_make "$WORK/empty" "$target"
  expect_status 1 "$target fails when $tool exits non-zero" \
    run_make "$(stub "$tool" 1)" "$target"
  expect_status 0 "$target passes when $tool exits zero" \
    run_make "$(stub "$tool" 0)" "$target"
  if [ -s "$WORK/$tool.log" ]; then
    note_pass "$target runs $tool"
  else
    note_fail "$target did not run $tool"
  fi
done

# 2. No tfsec in the security-scan chain.
if PATH="$WORK/empty" "$MAKE_BIN" --no-print-directory -C "$REPO_ROOT" -n security-scan 2>&1 | grep -q tfsec; then
  note_fail "security-scan still runs tfsec"
else
  note_pass "security-scan does not run tfsec"
fi

# 3. terraform-validate over the real environment roots.
roots=()
for d in "$REPO_ROOT"/terraform/environments/*/; do
  [ -d "$d" ] && roots+=("${d%/}")
done
if [ "${#roots[@]}" -eq 0 ]; then
  note_fail "no terraform/environments roots found to validate"
fi

expect_status 0 "terraform-validate passes when terraform exits zero" \
  run_make "$(stub terraform 0)" terraform-validate
for root in "${roots[@]}"; do
  if grep -qxF "$root init -backend=false -input=false" "$WORK/terraform.log" 2>/dev/null &&
     grep -qxF "$root validate" "$WORK/terraform.log" 2>/dev/null; then
    note_pass "terraform-validate inits and validates ${root#"$REPO_ROOT"/}"
  else
    note_fail "terraform-validate skipped ${root#"$REPO_ROOT"/}"
    sed 's/^/    /' "$WORK/terraform.log" 2>/dev/null || true
  fi
done
expect_status 1 "terraform-validate fails when terraform exits non-zero" \
  run_make "$(stub terraform 1)" terraform-validate
expect_status 1 "terraform-validate fails when terraform is missing" \
  run_make "$WORK/empty" terraform-validate

# 4. clean-locks, in a scratch tree so no real lock file is touched.
tree="$WORK/tree"
mkdir -p "$tree/terraform/environments/aws"
lock="$tree/terraform/environments/aws/.terraform.lock.hcl"
touch "$lock"
clean_locks() {
  "$MAKE_BIN" --no-print-directory -C "$tree" -f "$REPO_ROOT/Makefile.terraform" clean-locks "$@" > "$WORK/out.log" 2>&1
}
expect_status 1 "clean-locks refuses without CONFIRM_CLEAN_LOCKS=yes" clean_locks
if [ -f "$lock" ]; then
  note_pass "clean-locks left the lock file in place when refused"
else
  note_fail "clean-locks deleted the lock file without confirmation"
fi
expect_status 1 "clean-locks refuses CONFIRM_CLEAN_LOCKS=1" clean_locks CONFIRM_CLEAN_LOCKS=1
expect_status 0 "clean-locks runs with CONFIRM_CLEAN_LOCKS=yes" clean_locks CONFIRM_CLEAN_LOCKS=yes
if [ ! -e "$lock" ]; then
  note_pass "clean-locks deleted the lock file when confirmed"
else
  note_fail "clean-locks kept the lock file despite confirmation"
fi

# 5. ./... skips frontend/node_modules (go.mod's ignore directive). A probe
# package overlaid under frontend/node_modules must not be listed; a control
# probe overlaid elsewhere under frontend/ must be, which proves the overlay
# took effect and that the ignore covers node_modules only.
probe_src="$WORK/probe.go"
printf 'package probe\n\nfunc Probe() {}\n' > "$probe_src"
ignored_dir="frontend/node_modules/zz-make-gates-probe"
control_dir="frontend/zz-make-gates-control"
printf '{"Replace":{"%s":"%s","%s":"%s"}}\n' \
  "$REPO_ROOT/$ignored_dir/probe.go" "$probe_src" \
  "$REPO_ROOT/$control_dir/probe.go" "$probe_src" > "$WORK/overlay.json"
GO_BIN="$(command -v go || true)"
if [ -z "$GO_BIN" ]; then
  note_fail "go not found on PATH; cannot check that ./... skips node_modules"
else
  for tags in "" "integration"; do
    label="go list ./...${tags:+ -tags=$tags}"
    if ! (cd "$REPO_ROOT" && "$GO_BIN" list -e -tags="$tags" -overlay "$WORK/overlay.json" \
          -f '{{.Dir}}' ./...) > "$WORK/pkgs.log" 2> "$WORK/out.log"; then
      note_fail "$label failed"
      sed 's/^/    /' "$WORK/out.log"
      continue
    fi
    if grep -qxF "$REPO_ROOT/$control_dir" "$WORK/pkgs.log"; then
      note_pass "$label lists the control probe under frontend/"
    else
      note_fail "$label did not list the control probe (overlay not applied?)"
    fi
    if grep -qF "$REPO_ROOT/frontend/node_modules/" "$WORK/pkgs.log"; then
      note_fail "$label walks into frontend/node_modules"
      grep -F "$REPO_ROOT/frontend/node_modules/" "$WORK/pkgs.log" | sed 's/^/    /'
    else
      note_pass "$label skips frontend/node_modules"
    fi
  done
fi

echo ""
echo "Results: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
