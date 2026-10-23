#!/usr/bin/env bash
# scripts/run-gosec.sh
#
# Single source of truth for running gosec in this repository. The pre-commit
# hook (scripts/gosec-hook.sh), `make security-scan-go` and the CI gosec step
# all call this script, so a local pass predicts CI (issue #340).
#
# Rule set: gosec defaults, no -exclude. Suppress a reviewed false positive
# inline with `#nosec Gxxx -- reason`, never by excluding a rule here.
#
# Usage:
#   run-gosec.sh [--sarif-dir DIR] [--module MOD [PKG...]]...
#   run-gosec.sh --list-modules
#
# With no --module every module is scanned whole (./...). PKG paths are
# relative to the module root (for example ./internal/api); default ./...
# --sarif-dir writes DIR/gosec-<tag>.sarif instead of printing text findings,
# where <tag> is "root" for the root module and the module path with slashes
# turned into dashes otherwise (tests/e2e -> tests-e2e).
#
# The gosec version is read from .gosec-version at the repo root, the single
# source of truth that the Makefile's install-dev-tools also reads (issue
# #347). A missing or malformed file (anything but one vMAJOR.MINOR.PATCH line)
# is a setup failure, caught before Go runs or the cache is touched.
#
# Exit codes: 0 clean, 1 findings or a scan error, 2 setup/usage failure.
#
# The gosec binary is cached per (gosec version, effective Go version), because
# gosec cannot read export data produced by a newer toolchain than the one it
# was built with: such a binary scans nothing and reports 0 issues. The
# toolchain defaults to the go directive of the root go.mod, which matches the
# GO_VERSION that CI's setup-go installs.

set -euo pipefail

# Every module in go.work.
MODULES=(. tests/e2e)

if [[ "${1:-}" == "--list-modules" ]]; then
    printf '%s\n' "${MODULES[@]}"
    exit 0
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"

sarif_dir=""
declare -a scan_mods=() scan_pkgs=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --sarif-dir)
            [[ $# -ge 2 ]] || { echo "run-gosec: --sarif-dir needs a directory" >&2; exit 2; }
            sarif_dir="$2"; shift 2 ;;
        --module)
            [[ $# -ge 2 ]] || { echo "run-gosec: --module needs a module path" >&2; exit 2; }
            scan_mods+=("$2"); shift 2
            cur=""
            while [[ $# -gt 0 && "$1" != --* ]]; do
                case "$1" in
                    *[[:space:]]*) echo "run-gosec: package path contains whitespace: $1" >&2; exit 2 ;;
                esac
                cur+="${cur:+ }$1"
                shift
            done
            scan_pkgs+=("$cur") ;;
        *) echo "run-gosec: unknown argument: $1" >&2; exit 2 ;;
    esac
done

if [[ ${#scan_mods[@]} -eq 0 ]]; then
    scan_mods=("${MODULES[@]}")
    for _ in "${MODULES[@]}"; do scan_pkgs+=(""); done
fi

# Validate the pin before it reaches `go install` or the cache path: an empty
# or garbage value must not install some other gosec or escape the cache dir.
version_file="${REPO_ROOT}/.gosec-version"
[[ -f "$version_file" ]] || { echo "run-gosec: $version_file not found" >&2; exit 2; }
GOSEC_VERSION="$(cat "$version_file")"
if [[ ! "$GOSEC_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "run-gosec: $version_file must be one line like vMAJOR.MINOR.PATCH, got '${GOSEC_VERSION}'" >&2
    exit 2
fi

if [[ -z "${GOTOOLCHAIN:-}" ]]; then
    go_directive=$(awk '$1=="go"{print $2; exit}' "${REPO_ROOT}/go.mod")
    [[ -n "$go_directive" ]] || { echo "run-gosec: no go directive in go.mod" >&2; exit 2; }
    export GOTOOLCHAIN="go${go_directive}"
fi

# Key the cache on the Go version that actually builds gosec, not the requested
# string (GOTOOLCHAIN=local would otherwise hide a Go upgrade).
go_version=$(go env GOVERSION) || { echo "run-gosec: go env GOVERSION failed (is Go on PATH?)" >&2; exit 2; }
cache_root="${XDG_CACHE_HOME:-${HOME}/.cache}/gosec"
gosec_dir="${cache_root}/${GOSEC_VERSION}-${go_version}"
gosec_bin="${gosec_dir}/gosec"

if [[ ! -x "$gosec_bin" ]]; then
    echo "run-gosec: installing gosec@${GOSEC_VERSION} (${go_version}) -> ${gosec_dir}" >&2
    mkdir -p "$cache_root"
    # Install into a private dir and rename, so an interrupted or parallel
    # install never leaves a truncated binary at the trusted path.
    tmp_dir=$(mktemp -d "${cache_root}/.install.XXXXXX")
    if ! GOBIN="$tmp_dir" go install "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}"; then
        rm -rf "$tmp_dir"
        echo "run-gosec: install failed (is Go on PATH?)" >&2
        exit 2
    fi
    mkdir -p "$gosec_dir"
    mv -f "$tmp_dir/gosec" "$gosec_bin"
    rmdir "$tmp_dir"
fi

[[ -z "$sarif_dir" ]] || mkdir -p "$sarif_dir"

fail=0
for i in "${!scan_mods[@]}"; do
    mod="${scan_mods[$i]}"
    pkgs="${scan_pkgs[$i]:-./...}"
    args=(-exclude-dir=.legacy -exclude-dir=.dev-notes -exclude-dir=vendor)
    out=""
    if [[ -n "$sarif_dir" ]]; then
        tag="${mod//\//-}"
        [[ "$mod" == "." ]] && tag="root"
        out="${sarif_dir}/gosec-${tag}.sarif"
        rm -f "$out"
        args+=(-fmt sarif -out "$out")
    else
        # -quiet would also skip writing the SARIF file when there are no findings.
        args+=(-quiet)
    fi

    echo "gosec [${mod}]: scanning ${pkgs}" >&2
    # pkgs intentionally unquoted: space-separated package paths, no glob chars.
    # shellcheck disable=SC2086
    if ! (cd "${REPO_ROOT}/${mod}" && "$gosec_bin" "${args[@]}" $pkgs); then
        echo "::warning::gosec exited non-zero in ${mod} (findings or a scan error)" >&2
        fail=1
    fi
    if [[ -n "$out" && ! -f "$out" ]]; then
        echo "::error::gosec produced no SARIF output for ${mod}" >&2
        fail=1
    fi
done

exit $fail
