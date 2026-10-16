#!/usr/bin/env bash
# check-gcp-secret-scope.sh
#
# Fails when any Terraform file could grant a Secret Manager role at project,
# folder or organization scope. Those scopes hand the member EVERY secret in the
# scope, so a workload that needs one secret ends up able to read the credential
# encryption key, the JWT/session secrets and the SendGrid API key alongside it.
#
# The supported pattern is a per-secret binding instead:
#
#   resource "google_secret_manager_secret_iam_member" "..." {
#     project   = var.project_id
#     secret_id = var.some_secret_id
#     role      = "roles/secretmanager.secretAccessor"
#     member    = "serviceAccount:${...}"
#   }
#
# This guard exists because compute/gcp/cloud-run was migrated to per-secret
# bindings while its sibling compute/gcp/cleanup-function was left on a
# project-wide grant for as long as it took someone to notice (issue #1614).
#
# HOW IT WORKS. The guard does not try to follow a role from where it is written
# to the IAM resource that binds it: `locals`, `for_each` over sets and maps,
# `dynamic` blocks, `google_iam_policy` data sources, variable defaults, module
# arguments and tfvars all move the role name away from the resource, and an
# earlier version that only looked inside scope-wide IAM resource blocks was
# blind to every one of them (issue #149). Instead it works from the other end:
# a Secret Manager role or permission can only enter the configuration as text
# containing `secretmanager`, so EVERY such mention in a scanned file is a
# finding unless it is one of the shapes below.
#
#   1. Inside a per-secret IAM resource
#      (google_secret_manager_secret_iam_{member,binding,policy} and the
#      _regional_ variants), and only as a plain `role = "roles/secretmanager.X"`
#      attribute line. That is the supported pattern above.
#   2. A comment line (`#` or `//` as the first non-blank characters). A trailing
#      comment on a code line is NOT exempt; that is deliberate, so a `#` inside
#      a string can never hide the role written after it.
#   3. A `description` attribute: a whole-line `description = "..."` or a
#      `description = <<EOT` heredoc body. Descriptions are prose, never roles.
#   4. The API service name `secretmanager.googleapis.com`.
#   5. A line carrying an explicit waiver (see WAIVERS).
#
# Separately, a predefined role name ASSEMBLED at plan time
# (`"roles/${...}"`, `format("roles/%s", ...)`, a bare `"roles/"` or `"roles"`
# fragment) is a finding wherever it appears, because no static scanner can say
# which role it becomes. Refusing it is the fail-closed answer; the fix is to
# write the role name out.
#
# WAIVERS. A deliberate scope-wide Secret Manager grant is permitted by a comment
# on the line DIRECTLY above the mention it excuses, with a reason:
#
#   # gcp-secret-scope: allow - <why this grant must be scope-wide>
#   "roles/secretmanager.admin",
#
# It covers exactly that one following line. A waiver with no reason, a waiver
# whose next line has nothing to excuse, or a malformed `gcp-secret-scope:`
# comment is itself a finding, so a waiver can never silently match nothing.
# There is no allowlist file: the waiver sits next to the grant, moves and dies
# with it, and shows up in the diff that widens it.
#
# Exit 0 = no un-waived Secret Manager mention and no plan-time role name found.
# Exit 1 = at least one found; each is printed to stderr.
# Exit 2 = usage error, or a file this guard cannot read (JSON encoding).
#
# LIMITATIONS. This is a textual guard, not a policy engine.
#   - Values supplied from OUTSIDE the scanned tree are out of reach: `-var` on
#     the command line, tfvars files kept elsewhere, environment variables, and
#     callers of the `iac/` modules outside this repository. A variable's
#     in-tree default and every in-tree module argument or tfvars file ARE
#     covered, because the role text has to be written in one of them.
#   - It over-reports rather than under-reports. A per-secret binding whose role
#     comes from a local or a for_each (rather than a literal on its `role =`
#     line) is reported; write the literal, or waive it.
#   - The Terraform JSON encoding (`.tf.json`, `.tfvars.json`) is not parsed.
#     Rather than report such a file as clean, the guard exits 2 if it finds one.
#     None exist today.
#
# Usage:
#   scripts/check-gcp-secret-scope.sh              # scan the default roots
#   scripts/check-gcp-secret-scope.sh FILE...      # scan exactly these files
#
# Passing explicit files is how the test harness points the check at fixtures
# without touching the real sources.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Every directory holding Terraform this guard should cover. `iac/` carries the
# customer-facing federation modules, which are exactly the kind of thing that
# must not ship a scope-wide grant; scanning only `terraform/` missed all 20 of
# its .tf files.
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
    # NUL-delimited so paths with spaces survive. tfvars files are scanned
    # because they are one of the places a role list can be supplied from. The
    # JSON encodings are collected so they can be REJECTED below, not because
    # they can be parsed: silently returning "clean" for a file the scanner
    # cannot read is the exact failure mode this guard exists to prevent.
    while IFS= read -r -d '' f; do
      files+=("$f")
    done < <(find "$root" -type f \( -name '*.tf' -o -name '*.tf.json' \
                                    -o -name '*.tfvars' -o -name '*.tfvars.json' \
                                    -o -name '*.tfvars.example' \) -print0 | sort -z)
  done
fi

# Fail closed on the JSON encoding rather than under-reporting it. None exist in
# the repo today; if one is ever added, this stops rather than passing it over.
json_files=()
for f in "${files[@]}"; do
  [[ "$f" == *.tf.json || "$f" == *.tfvars.json ]] && json_files+=("$f")
done
if [[ ${#json_files[@]} -gt 0 ]]; then
  {
    echo "ERROR: this guard cannot parse the Terraform JSON encoding, and will not"
    echo "       report a file it cannot read as clean. Found:"
    printf '         %s\n' "${json_files[@]}"
    echo "       Extend the scanner to handle the JSON encoding before adding one."
  } >&2
  exit 2
fi

if [[ ${#files[@]} -eq 0 ]]; then
  echo "ERROR: no Terraform files to scan" >&2
  exit 2
fi

# Line scanner. Portable awk only (CI runs mawk, macOS runs BWK awk): no gawk
# extensions, no regex intervals, braces matched via bracket expressions.
#
# Block tracking exists for exactly one purpose: deciding whether a line is
# inside a per-secret IAM resource, which is the only place a role mention is
# exempt (shape 1). Every way that tracking can go wrong is made to fail closed:
#   - Depth counts every `{` and `}` outside heredocs. Interpolations are
#     balanced, so only an unbalanced brace inside a string can skew it.
#   - Inside a per-secret resource, the exemption is lost for the rest of the
#     block as soon as anything that could open another top-level construct
#     appears (`resource`, `data`, `locals`, `module`, `variable`, `output`,
#     `dynamic`, ...), so a skewed depth cannot carry the exemption onto a
#     following block.
#   - A `}` in column 0 ends the exemption whatever the count says.
#   - Even inside the exemption, only a plain `role = "roles/secretmanager.X"`
#     line is exempt; any other Secret Manager mention there is still reported.
violations=$(
  awk '
    function report(file, line, why, text) {
      sub(/^[[:space:]]+/, "", text)
      printf "%s:%d: %s\n      %s\n", file, line, why, text
    }

    function flush_waiver() {
      if (pending) {
        report(pending_file, pending_line,
               "waiver excuses nothing: the line directly below it has no Secret Manager mention",
               pending_text)
      }
      pending = 0
    }

    # Returns the reason this line is a finding, or "" when it is not.
    function taint(s,    t) {
      t = tolower(s)
      gsub(/secretmanager\.googleapis\.com/, "", t)
      if (t ~ /secretmanager/) {
        if (allowed && s ~ /^[[:space:]]*role[[:space:]]*=[[:space:]]*"roles\/secretmanager\.[A-Za-z]+"[[:space:]]*$/) {
          return ""
        }
        return "Secret Manager role or permission outside a per-secret IAM resource"
      }
      if (t ~ /"roles\/[^"]*([$][{]|%)/ || t ~ /"roles\/?"/) {
        return "role name assembled at plan time; cannot be checked statically, write it out"
      }
      return ""
    }

    FNR == 1 {
      flush_waiver()
      depth = 0; allowed = 0
      in_heredoc = 0; heredoc_desc = 0; heredoc_marker = ""
    }

    in_heredoc {
      t = $0
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", t)
      if (t == heredoc_marker) {
        in_heredoc = 0
        if (pending) flush_waiver()
        next
      }
      if (heredoc_desc) {
        if (pending) flush_waiver()
        next
      }
      why = taint($0)
      if (pending) {
        if (why != "") { pending = 0; next }
        flush_waiver()
      }
      if (why != "") report(FILENAME, FNR, why, $0)
      next
    }

    # Blank and comment-only lines. A waiver must be immediately above the line
    # it excuses, so one of these arriving while a waiver is pending means the
    # waiver excuses nothing.
    /^[[:space:]]*$/ || /^[[:space:]]*(#|\/\/)/ {
      if (pending) flush_waiver()
      if ($0 ~ /(^|[^-A-Za-z0-9_])gcp-secret-scope[[:space:]]*:/) {
        if ($0 ~ /^[[:space:]]*(#|\/\/)[[:space:]]*gcp-secret-scope:[[:space:]]*allow[[:space:]]+-[[:space:]]*[^[:space:]]/) {
          pending = 1; pending_file = FILENAME; pending_line = FNR; pending_text = $0
        } else {
          report(FILENAME, FNR,
                 "malformed waiver: expected \"# gcp-secret-scope: allow - <reason>\"", $0)
        }
      }
      next
    }

    {
      line = $0
      opens_construct = (line ~ /^[[:space:]]*(resource|data|module|locals|variable|output|provider|terraform|dynamic|moved|import|check|removed)([[:space:]]|[{]|$)/)

      if (depth == 0 && opens_construct) {
        allowed = (line ~ /^[[:space:]]*resource[[:space:]]+"google_secret_manager_(regional_)?secret_iam_(member|binding|policy)"/)
      } else if (allowed && opens_construct) {
        allowed = 0
      }

      # Whole-line single-string description: prose, not a role.
      if (line ~ /^[[:space:]]*description[[:space:]]*=[[:space:]]*"[^"]*"[[:space:]]*$/) {
        why = ""
      } else if (opens_construct) {
        # Block labels name things (`resource "google_project_service"
        # "secretmanager"`, `output "roles"`); they are not values. Only what
        # follows them, e.g. the body of a one-line block, is checked.
        body = line
        sub(/^[[:space:]]*[a-z_]+([[:space:]]+"[^"]*")*/, "", body)
        why = taint(body)
      } else {
        why = taint(line)
      }

      if (pending) {
        if (why != "") { pending = 0; why = "" }
        else flush_waiver()
      }
      if (why != "") report(FILENAME, FNR, why, line)

      # Heredoc opener. The body is scanned like any other value (a policy JSON
      # heredoc can carry a role), except a description heredoc, which is prose.
      if (match(line, /<<-?[A-Za-z_][A-Za-z0-9_]*[[:space:]]*$/)) {
        heredoc_marker = substr(line, RSTART, RLENGTH)
        sub(/^<<-?/, "", heredoc_marker)
        gsub(/[[:space:]]+$/, "", heredoc_marker)
        heredoc_desc = (line ~ /^[[:space:]]*description[[:space:]]*=[[:space:]]*<</)
        in_heredoc = 1
      }

      opens = gsub(/[{]/, "{", line)
      closes = gsub(/[}]/, "}", line)
      depth += opens - closes
      # A `}` in column 0 is how `terraform fmt` closes a top-level block, so it
      # ends the exemption too, even if a skewed count says otherwise.
      if (depth <= 0 || line ~ /^}/) { depth = 0; allowed = 0 }
    }

    END { flush_waiver() }
  ' "${files[@]}"
)

if [[ -n "$violations" ]]; then
  {
    echo "FAILED: possible scope-wide Secret Manager grant(s) found."
    echo ""
    echo "$violations"
    echo ""
    echo "A project/folder/organization-scoped Secret Manager role grants the member"
    echo "every secret in that scope. Replace it with a per-secret binding:"
    echo ""
    echo "  resource \"google_secret_manager_secret_iam_member\" \"<name>\" {"
    echo "    project   = var.project_id"
    echo "    secret_id = var.<the_one_secret_this_workload_reads>"
    echo "    role      = \"roles/secretmanager.secretAccessor\""
    echo "    member    = \"serviceAccount:\${...}\""
    echo "  }"
    echo ""
    echo "See terraform/modules/compute/gcp/cloud-run/main.tf for the reference shape."
    echo "If the grant must be scope-wide, put a waiver with the reason on the line"
    echo "directly above it:"
    echo ""
    echo "  # gcp-secret-scope: allow - <why this grant must be scope-wide>"
    echo ""
    echo "See the header of scripts/check-gcp-secret-scope.sh for what is exempt."
  } >&2
  exit 1
fi

echo "OK: no un-waived Secret Manager role or permission outside a per-secret IAM" \
     "resource, and no plan-time role name, in ${#files[@]} Terraform file(s)." \
     "Values supplied from outside the scanned tree are not covered; see LIMITATIONS."
