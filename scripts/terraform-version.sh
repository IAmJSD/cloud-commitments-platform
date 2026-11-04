#!/usr/bin/env bash
# terraform-version.sh
#
# Prints the Terraform version pinned in .terraform-version (repo root, one
# line such as `1.10.5`), the single source of truth for every workflow that
# installs Terraform (issue #349). When GITHUB_OUTPUT is set, it also writes
# `version=<X.Y.Z>` there, so a workflow step reads it with:
#
#   - name: Read Terraform version
#     id: terraform-version
#     run: bash scripts/terraform-version.sh
#   - uses: hashicorp/setup-terraform@...
#     with:
#       terraform_version: ${{ steps.terraform-version.outputs.version }}
#
# The format is validated rather than passed through: setup-terraform treats
# an empty terraform_version as `latest` and accepts constraint strings, so a
# missing, empty or malformed file would otherwise install an unpinned
# Terraform without failing anything.
#
# tfenv and similar version managers read the same file locally.
#
# Usage: terraform-version.sh [ROOT]   (ROOT defaults to this repository)
# Exit codes: 0 ok, 1 missing or malformed file, 2 usage error.

set -euo pipefail

if [[ $# -gt 1 ]]; then
  echo "Usage: $0 [ROOT]" >&2
  exit 2
fi

ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
VERSION_FILE="${ROOT}/.terraform-version"

if [[ ! -f "$VERSION_FILE" ]]; then
  echo "::error::.terraform-version is missing in ${ROOT}" >&2
  exit 1
fi

version="$(cat "$VERSION_FILE")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::.terraform-version must be one line of the form MAJOR.MINOR.PATCH, got '${version}'" >&2
  exit 1
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "version=${version}" >> "$GITHUB_OUTPUT"
fi
echo "$version"
