#!/usr/bin/env bash
# read-tf-outputs.sh KEY=OUTPUT_NAME [KEY=OUTPUT_NAME ...]
#
# Reads Terraform outputs from the working directory with
# `terraform output -raw OUTPUT_NAME` and appends them to $GITHUB_OUTPUT as
# KEY=value step outputs. Run it from the Terraform root module directory,
# after the apply.
#
# It fails loudly, at the source, instead of handing an empty value to a later
# job (#438). The inline form it replaces in deploy-azure.yml was
#
#   APP_URL=$(terraform output -raw container_app_url 2>/dev/null | grep -v '::' || echo "")
#
# which hid terraform's error message and turned any failure (missing output,
# state read error, renamed output) into an empty step output. The failure then
# surfaced jobs later, as "Failed to get app URL" or "Terraform outputs
# missing", far from its cause.
#
# Wrapper noise: hashicorp/setup-terraform installs a `terraform` wrapper that
# writes `::debug::` workflow-command lines to stdout next to the real output
# (3d0d9dee). Lines containing `::` are dropped, as before; what is left must
# be exactly one non-empty line. Anything else (nothing left, or several lines)
# is an error that prints what terraform returned, rather than a guess.
#
# Every output is read before anything is written, so a failure leaves
# $GITHUB_OUTPUT untouched rather than half-written.
#
# Exit codes:
#   0  every output was read and written
#   1  terraform failed, or an output was empty or not a single line
#   2  usage error (no arguments, malformed KEY=OUTPUT_NAME, GITHUB_OUTPUT unset)

set -euo pipefail

usage() {
  echo "usage: $(basename "$0") KEY=OUTPUT_NAME [KEY=OUTPUT_NAME ...]" >&2
  echo "  appends KEY=<terraform output -raw OUTPUT_NAME> to \$GITHUB_OUTPUT" >&2
  exit 2
}

[[ $# -ge 1 ]] || usage
if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
  echo "::error::GITHUB_OUTPUT is not set" >&2
  exit 2
fi

ident='^[A-Za-z_][A-Za-z0-9_]*$'
for pair in "$@"; do
  key="${pair%%=*}"
  name="${pair#*=}"
  if [[ "$pair" != *=* || ! "$key" =~ $ident || ! "$name" =~ $ident ]]; then
    echo "::error::malformed argument '${pair}', expected KEY=OUTPUT_NAME" >&2
    exit 2
  fi
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

lines=()
for pair in "$@"; do
  key="${pair%%=*}"
  name="${pair#*=}"

  # stderr is left alone so terraform's own error message reaches the log.
  if ! terraform output -raw "$name" > "$tmp/raw"; then
    echo "::error::terraform output -raw ${name} failed; see the terraform error above" >&2
    exit 1
  fi

  # `|| true`: grep exits 1 when it selects nothing, which is judged below.
  # grep also terminates the last line, which -raw leaves without a newline.
  grep -v '::' "$tmp/raw" | grep -v '^[[:space:]]*$' > "$tmp/value" || true
  count=$(wc -l < "$tmp/value")

  if ((count != 1)); then
    if ((count == 0)); then
      reason="is empty"
    else
      reason="has ${count} lines, expected one"
    fi
    echo "::error::terraform output ${name} ${reason}. Raw stdout:" >&2
    sed 's/^/  | /' "$tmp/raw" >&2
    exit 1
  fi

  lines+=("${key}=$(cat "$tmp/value")")
  echo "Read terraform output ${name} into ${key}"
done

printf '%s\n' "${lines[@]}" >> "$GITHUB_OUTPUT"
