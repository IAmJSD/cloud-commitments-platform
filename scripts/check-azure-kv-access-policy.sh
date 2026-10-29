#!/usr/bin/env bash
# check-azure-kv-access-policy.sh
#
# Fails when any Terraform file declares an Azure Key Vault access-policy grant,
# in any of its three spellings:
#
#   resource "azurerm_key_vault_access_policy" "..." { ... }
#   resource "azurerm_key_vault" "..." { access_policy { ... } }
#   resource "azurerm_key_vault" "..." { dynamic "access_policy" { ... } }
#
# WHY A BLANKET BAN IS CORRECT HERE. This repo provisions exactly one Key Vault
# (terraform/modules/secrets/azure/main.tf) and it hardcodes
# `enable_rbac_authorization = true` as a literal, not a variable. An
# RBAC-enabled vault NEVER consults its accessPolicies array: data-plane
# authorization comes from Azure RBAC role assignments and nothing else.
#
# The failure mode this guards is that Azure's control plane happily ACCEPTS the
# access-policy write, so `terraform apply` reports success with no warning and
# the grant is silently inert. It surfaces only as a runtime 403, which is the
# worst possible place to discover it. That is exactly what happened to the
# Azure cleanup-function (since deleted, #357) and the AKS workload identity
# (issue #1621) while every other consumer in the tree already used
# `azurerm_role_assignment`.
#
# The supported pattern is:
#
#   resource "azurerm_role_assignment" "..." {
#     scope                = var.key_vault_id
#     role_definition_name = "Key Vault Secrets User"
#     principal_id         = <identity principal id>
#   }
#
# Pick the role by mapping the access-policy permissions you would have written:
#   secret_permissions Get/List          -> "Key Vault Secrets User"
#   secret_permissions incl. Set/Delete  -> "Key Vault Secrets Officer"
#   key_permissions Sign/Get             -> "Key Vault Crypto User"
# Verify the role exists before using it (`az role definition list --name ...`);
# a matching string in a sibling file is not evidence that a role or an action
# is real (issue #1794).
#
# Exit 0 = no access-policy grant declared, in any of the three spellings.
# Exit 1 = at least one found; each is printed to stderr.
# Exit 2 = usage error, or a file this scanner cannot read or parse.
#
# HOW IT DECIDES. The files are parsed with HashiCorp's HCL parser (the one
# Terraform uses), by cmd/check-azure-kv-access-policy, rather than matched line
# by line. An `access_policy` block is a Key Vault grant only when the resource
# enclosing it is an `azurerm_key_vault`; other providers' resources
# (azurerm_storage_share's acl, for one) have an unrelated block of that name,
# and the parse names the enclosing type directly. It also means text inside a
# comment or a heredoc is never read as a declaration, block headers are found
# at any indentation and with any token spacing, and attributes such as
# `access_policy_enabled = false` are not blocks at all. Four successive line
# matchers each got one of those cases wrong (issue #204).
#
# LIMITATIONS. Only the direct children of an azurerm_key_vault are checked,
# which is where the provider schema puts access_policy. The attribute spelling
# of the same argument (`access_policy = [...]`, the provider's
# attributes-as-blocks mode, whose documented use is `= []` to clear every
# policy) is not reported. Modules are checked as source files, not expanded.
#
# A file the parser rejects is reported as exit 2 (cannot check), never clean.
# The Terraform JSON encoding (`.tf.json`) is not parsed either; rather than
# report such a file as clean, the guard exits 2. None exist today.
#
# The parser needs Go on PATH (the version in go.mod); without it the guard
# exits 2.
#
# Usage:
#   scripts/check-azure-kv-access-policy.sh              # scan the default roots
#   scripts/check-azure-kv-access-policy.sh FILE...      # scan exactly these files
#
# Passing explicit files is how the test harness points the check at fixtures
# without touching the real sources.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Every directory holding Terraform this guard should cover. `iac/` carries the
# customer-facing federation modules; they do not provision a vault today, but
# scanning only `terraform/` would let one arrive there unnoticed.
SCAN_ROOTS=(
  "${REPO_ROOT}/terraform"
  "${REPO_ROOT}/iac"
)

files=()
if [[ $# -gt 0 ]]; then
  for arg in "$@"; do
    case "$arg" in
      -*) echo "Unknown flag: $arg" >&2; exit 2 ;;
    esac
    if [[ ! -f "$arg" ]]; then
      echo "ERROR: file not found: $arg" >&2
      exit 2
    fi
    files+=("$arg")
  done
else
  for root in "${SCAN_ROOTS[@]}"; do
    if [[ ! -d "$root" ]]; then
      echo "ERROR: scan root not found: $root" >&2
      exit 2
    fi
    # NUL-delimited so paths with spaces survive. `.tf.json` is collected so it
    # can be REJECTED below, not because it can be parsed: silently returning
    # "clean" for a file it cannot read is the exact failure mode this guard
    # exists to prevent.
    while IFS= read -r -d '' f; do
      files+=("$f")
    done < <(find "$root" -type f \( -name '*.tf' -o -name '*.tf.json' \) -print0 | sort -z)
  done
fi

# Fail closed on the JSON encoding rather than under-reporting it.
json_files=()
for f in "${files[@]}"; do
  [[ "$f" == *.tf.json ]] && json_files+=("$f")
done
if [[ ${#json_files[@]} -gt 0 ]]; then
  {
    echo "ERROR: this guard cannot parse the Terraform JSON encoding, and will not"
    echo "       report a file it cannot read as clean. Found:"
    printf '         %s\n' "${json_files[@]}"
    echo "       Extend the scanner to handle .tf.json before adding one."
  } >&2
  exit 2
fi

if [[ ${#files[@]} -eq 0 ]]; then
  echo "ERROR: no Terraform files to scan" >&2
  exit 2
fi

if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: go is not on PATH; the HCL parser this guard runs is a Go program" >&2
  exit 2
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
checker="${work_dir}/check-azure-kv-access-policy"
if ! (cd "$REPO_ROOT" && go build -o "$checker" ./cmd/check-azure-kv-access-policy); then
  echo "ERROR: could not build cmd/check-azure-kv-access-policy" >&2
  exit 2
fi

# The checker prints one FILE:LINE: HEADER line per grant on stdout, and exits
# 0 (none), 1 (found) or 2 (cannot check). Anything other than 0 or 1 is passed
# on as 2, so a crash can never read as a pass or as a finding.
checker_exit=0
violations="$("$checker" "${files[@]}")" || checker_exit=$?
case "$checker_exit" in
  0|1) ;;
  *) exit 2 ;;
esac

if [[ "$checker_exit" -eq 1 ]]; then
  {
    echo "FAILED: Azure Key Vault access policy declared."
    echo ""
    echo "$violations"
    echo ""
    echo "This project's Key Vault sets enable_rbac_authorization = true"
    echo "(terraform/modules/secrets/azure/main.tf). An RBAC-enabled vault ignores"
    echo "access policies entirely, so the grant above applies cleanly and then"
    echo "does nothing at runtime. That holds for a standalone resource and for an"
    echo "access_policy block inside the vault alike. Remove it and use an RBAC"
    echo "role assignment instead:"
    echo ""
    echo "  resource \"azurerm_role_assignment\" \"<name>\" {"
    echo "    scope                = var.key_vault_id"
    echo "    role_definition_name = \"Key Vault Secrets User\"  # Get + List"
    echo "    principal_id         = <identity principal id>"
    echo "  }"
    echo ""
    echo "See terraform/modules/compute/azure/container-apps/scheduled-tasks.tf"
    echo "for the reference shape, and confirm any role name you pick actually"
    echo "exists with: az role definition list --name \"<role>\""
  } >&2
  exit 1
fi

echo "OK: no Key Vault access policy (resource or nested block) declared in ${#files[@]} Terraform file(s)."
