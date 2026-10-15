#!/usr/bin/env bash
set -euo pipefail

parity=false
if [[ $# -eq 1 && "$1" == --docker-parity ]]; then
  parity=true
elif [[ $# -ne 0 ]]; then
  echo "Usage: $0 [--docker-parity]" >&2
  exit 2
fi

root="$(git rev-parse --show-toplevel)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/actionlint-hook.XXXXXXXX")"
trap 'printf "actionlint test evidence: %s\n" "$scratch"' EXIT
mkdir -p "$scratch/repo/scripts" "$scratch/repo/.github/workflows"
cp "$root/scripts/actionlint-hook.py" "$scratch/repo/scripts/"
python3 -c 'import sys,yaml; c=yaml.safe_load(open(sys.argv[1])); h=next(h for r in c["repos"] if r["repo"]=="local" for h in r["hooks"] if h["id"]=="actionlint"); yaml.safe_dump({"repos":[{"repo":"local","hooks":[h]}]},open(sys.argv[2],"w"))' \
  "$root/.pre-commit-config.yaml" "$scratch/repo/hook config.yaml"
git -C "$scratch/repo" init -q
export PRE_COMMIT_HOME="$scratch/pre-commit"
export GOMODCACHE="$scratch/modules"

expect() {
  local wanted="$1" diagnostic="$2" log="$3"
  shift 3
  local status=0
  "$@" >"$log" 2>&1 || status=$?
  if [[ "$wanted" == pass && "$status" -ne 0 ]] || [[ "$wanted" == fail && "$status" -eq 0 ]]; then
    cat "$log" >&2
    echo "Unexpected $wanted result: $* (status $status)" >&2
    exit 1
  fi
  if [[ -n "$diagnostic" ]] && ! grep -Fq -- "$diagnostic" "$log"; then
    cat "$log" >&2
    echo "Missing diagnostic: $diagnostic" >&2
    exit 1
  fi
}

run_hook() (
  cd "$scratch/repo"
  pre-commit run --config 'hook config.yaml' actionlint --files "$@"
)

for fixture in clean shell python syntax; do
  name=".github/workflows/$fixture workflow.yml"
  cp "$root/scripts/testdata/actionlint-hook/$fixture.fixture" "$scratch/repo/$name"
  wanted=fail
  case "$fixture" in
    clean) wanted=pass; diagnostic='Passed' ;;
    shell) diagnostic='SC2086' ;;
    python) diagnostic="undefined name 'undefined_name'" ;;
    syntax) diagnostic='[syntax-check]' ;;
  esac
  expect "$wanted" "$diagnostic" "$scratch/$fixture.native.log" run_hook "$name"
  if "$parity"; then
    image='rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667'
    docker_diagnostic="$diagnostic"
    if [[ "$wanted" == pass ]]; then docker_diagnostic=''; fi
    expect "$wanted" "$docker_diagnostic" "$scratch/$fixture.docker.log" \
      docker run --rm -v "$scratch/repo:/repo" -w /repo "$image" "$name"
  fi
done

cp "$root/scripts/testdata/actionlint-hook/clean.fixture" "$scratch/repo/.github/workflows/clean workflow.yaml"
expect pass 'Passed' "$scratch/yaml.native.log" run_hook '.github/workflows/clean workflow.yaml'
if "$parity"; then
  expect pass '' "$scratch/yaml.docker.log" docker run --rm -v "$scratch/repo:/repo" -w /repo "$image" '.github/workflows/clean workflow.yaml'
fi

interpreters=("$PRE_COMMIT_HOME"/repo*/py_env-python3.12/bin/python)
[[ ${#interpreters[@]} -eq 1 && -x "${interpreters[0]}" ]]
python="${interpreters[0]}"
tools="${python%/python}"
wrapper="$scratch/repo/scripts/actionlint-hook.py"
clean="$scratch/repo/.github/workflows/clean workflow.yaml"
expect pass '' "$scratch/warm-offline.log" env GOPROXY=off "$python" "$wrapper" "$clean"
expect fail 'timed out waiting for bootstrap lock' "$scratch/lock-timeout.log" "$python" -c \
  'import fcntl,importlib.util,signal,sys; from pathlib import Path; spec=importlib.util.spec_from_file_location("hook",sys.argv[1]); hook=importlib.util.module_from_spec(spec); spec.loader.exec_module(hook); hook.LOCK_TIMEOUT_SECONDS=0.1; lock=(Path(sys.executable).parent/".actionlint-install.lock").open("a"); fcntl.flock(lock,fcntl.LOCK_EX); signal.alarm(2); sys.exit(hook.main())' "$wrapper"
expect fail 'Go is required' "$scratch/missing-go.log" env PATH=/nonexistent "$python" "$wrapper" "$clean"

python3.12 -m venv --without-pip "$scratch/bootstrap"
bootstrap="$scratch/bootstrap/bin"
ln -s "$tools/shellcheck" "$bootstrap/shellcheck"
ln -s "$tools/pyflakes" "$bootstrap/pyflakes"
expect fail 'module lookup disabled by GOPROXY=off' "$scratch/cold-offline.log" \
  env GOPROXY=off GOMODCACHE="$scratch/empty-modules" "$bootstrap/python" "$wrapper" "$clean"
"$bootstrap/python" "$wrapper" "$clean" > "$scratch/concurrent-one.log" 2>&1 &
one=$!
"$bootstrap/python" "$wrapper" "$clean" > "$scratch/concurrent-two.log" 2>&1 &
two=$!
status=0
wait "$one" || status=1
wait "$two" || status=1
[[ "$status" -eq 0 ]]
expect pass '' "$scratch/concurrent-verified.log" env GOPROXY=off "$bootstrap/python" "$wrapper" "$clean"
cp "$(command -v go)" "$bootstrap/actionlint"
expect fail 'cached binary does not match the pinned module' "$scratch/wrong-actionlint.log" "$bootstrap/python" "$wrapper" "$clean"
printf 'corrupt cached binary\n' > "$bootstrap/actionlint"
expect fail 'cannot provision or verify pinned actionlint' "$scratch/corrupt-actionlint.log" "$bootstrap/python" "$wrapper" "$clean"
mv "$bootstrap/actionlint" "$bootstrap/actionlint.corrupt"
ln -s "$scratch/outside-actionlint" "$bootstrap/actionlint"
expect fail 'cached actionlint must not be a symlink' "$scratch/broken-link.log" "$bootstrap/python" "$wrapper" "$clean"
[[ ! -e "$scratch/outside-actionlint" ]]
cp "$tools/actionlint" "$scratch/outside-actionlint"
expect fail 'cached actionlint must not be a symlink' "$scratch/valid-link.log" "$bootstrap/python" "$wrapper" "$clean"
cmp "$tools/actionlint" "$scratch/outside-actionlint"

python3.12 -m venv --without-pip "$scratch/probe"
probe="$scratch/probe/bin"
expect fail 'missing required hook dependency shellcheck' "$scratch/missing-shellcheck.log" "$probe/python" "$wrapper" "$clean"
ln -s "$tools/shellcheck" "$probe/shellcheck"
expect fail 'missing required hook dependency pyflakes' "$scratch/missing-pyflakes.log" "$probe/python" "$wrapper" "$clean"
"$python" -c 'import pathlib,sys; p=pathlib.Path(sys.argv[1]); p.write_text("#!/bin/sh\nexit 9\n"); p.chmod(0o755)' "$probe/pyflakes"
expect fail 'cannot verify pyflakes' "$scratch/corrupt-pyflakes.log" "$probe/python" "$wrapper" "$clean"
"$python" -c 'import pathlib,sys; p=pathlib.Path(sys.argv[1]); p.write_text("#!/bin/sh\nprintf \"0.0.0 Python\\n\"\n"); p.chmod(0o755)' "$probe/pyflakes"
expect fail 'required pyflakes 3.4.0' "$scratch/wrong-version.log" "$probe/python" "$wrapper" "$clean"

printf 'self-hosted-runner:\n  labels: [custom-transport-runner]\n' > "$scratch/config with spaces.yaml"
"$python" -c 'from pathlib import Path; import sys; Path(sys.argv[2]).write_text(Path(sys.argv[1]).read_text().replace("ubuntu-latest", "custom-transport-runner"))' "$clean" "$scratch/custom runner.yaml"
expect fail '[runner-label]' "$scratch/config-control.native.log" "$python" "$wrapper" "$scratch/custom runner.yaml"
expect pass '' "$scratch/config.native.log" "$python" "$wrapper" -config-file "$scratch/config with spaces.yaml" "$scratch/custom runner.yaml"
expect fail 'SC2086' "$scratch/debug.native.log" "$python" "$wrapper" -debug "$scratch/repo/.github/workflows/shell workflow.yml"
grep -Fq -- '--norc -f json -x --shell bash -e SC1091,SC2194,SC2050,SC2153,SC2154,SC2157,SC2043 -' "$scratch/debug.native.log"
expect fail "undefined name 'undefined_name'" "$scratch/debug-python.native.log" "$python" "$wrapper" -debug "$scratch/repo/.github/workflows/python workflow.yml"
grep -Eq 'Running .*/pyflakes for Python script:' "$scratch/debug-python.native.log"
printf 'actionlint transport regression checks passed\n'
