#!/usr/bin/env bash
# check-aws-iam-parity.sh
#
# Asserts that the AWS IAM action lists granted to the CUDly runtime and to the
# customer-deployed federation roles are identical across every IaC flavor that
# encodes them, so deployments of the same code through different flavors
# cannot silently drift apart (the AWS sibling of check-azure-role-parity.sh).
#
# Three comparisons:
#
#   1. runtime    : terraform/modules/compute/aws/lambda/main.tf
#                   == terraform/modules/compute/aws/fargate/main.tf
#      (the CloudFormation install stack that used to be a third runtime copy
#      was retired in #1432)
#   2. federation cross-account pair (incl. the optional org-discovery
#      statement, which exists in both as an opt-in):
#                   iac/federation/aws-cross-account/cloudformation/template.yaml
#                   == iac/federation/aws-cross-account/terraform/main.tf
#   3. federation core (org discovery excluded: the CLI quick-onboarding
#      scripts and the aws-target WIF flavor intentionally do not offer it):
#                   both files from (2)
#                   == iac/federation/aws-target/cloudformation/template.yaml
#                   == iac/federation/aws-target/terraform/main.tf
#                   == internal/iacfiles/templates/aws-cross-account-cli.sh.tmpl
#                   == internal/iacfiles/templates/aws-wif-cli.sh.tmpl
#
# Only actions in the cloud-API namespaces the application code calls are
# compared (see ACTION_PREFIXES). Platform plumbing (logs, dynamodb, ses, sns,
# secretsmanager, sts, ssmmessages, lambda) legitimately differs per deployment
# flavor and is excluded.
#
# Exit 0 = all lists match.
# Exit 1 = drift found; the diff is printed to stderr.
# Exit 2 = usage / environment error.
#
# Usage:
#   scripts/check-aws-iam-parity.sh [--root <path>]
#
# --root lets the test harness point at a fixture tree that mirrors the repo
# layout without touching the real sources.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --root)
      if [[ $# -lt 2 || -z "${2:-}" ]]; then
        echo "ERROR: --root requires a path value" >&2
        exit 2
      fi
      REPO_ROOT="$2"
      shift 2
      ;;
    *) echo "Unknown flag: $1" >&2; exit 2 ;;
  esac
done

# IAM action namespaces owned by the application code. Keep in sync with the
# services the providers/aws code actually calls.
ACTION_PREFIXES='ce|ec2|rds|elasticache|es|redshift|memorydb|savingsplans|organizations'

# --- extraction ---------------------------------------------------------------
# Pull every token shaped like <prefix>:<CamelCaseAction> out of a file,
# regardless of whether it is YAML, HCL, or an embedded JSON heredoc. IAM
# actions always start with an uppercase letter or a "*" after the colon, which
# keeps ARNs (region segments are lowercase) out of the match. Wildcards
# (<prefix>:*, <prefix>:Describe*, and a bare Action "*") are extracted as
# their own tokens so widening one side of a pair is reported as drift.

# bare_star_action prints "*" when stdin grants a bare "*" Action. The files
# are YAML, HCL and JSON heredocs with no common parser in this script's
# toolset, so this awk follows each Action/Actions value across lines:
#   - a scalar:                 Action: "*"   "Action": "*"   Action = "*"
#   - a flow, JSON or HCL list: actions = ["*"]   "Action": [\n  "*"\n]
#   - a YAML block list:        Action:\n  - "*"
# Only a quoted lone "*" counts ("s3:*" does not; the token regex above
# handles service wildcards). The key must start the line or follow "{" or
# ",", so NotAction / not_actions (a "*" there matches no action and grants
# nothing), Resource "*", Condition values and prose do not count.
# POSIX awk only (CI runs mawk): no IGNORECASE, no [[:space:]] reliance.
bare_star_action() {
  awk '
    # scan walks one line of a value from bracket depth d. It sets FOUND on a
    # quoted lone "*", DONE when the value ends (REST = text after it), and
    # returns the depth still open at the end of the line.
    function scan(s, d,    i, n, c, q, buf, seen) {
      DONE = 0; REST = ""; q = ""; seen = 0; n = length(s)
      for (i = 1; i <= n; i++) {
        c = substr(s, i, 1)
        if (q != "") {
          if (c == "\\" && q == "\"") { buf = buf c substr(s, i + 1, 1); i++ }
          else if (c != q) buf = buf c
          else {
            q = ""
            if (buf == "*") FOUND = 1
            if (d == 0) { DONE = 1; REST = substr(s, i + 1); return 0 }
          }
          continue
        }
        if (c == "\"" || c == "\047") { q = c; buf = ""; seen = 1 }
        else if (c == "#" || (c == "/" && substr(s, i + 1, 1) == "/")) break
        else if (c == "[") { d++; seen = 1 }
        else if (c == "]") { d--; if (d <= 0) { DONE = 1; REST = substr(s, i + 1); return 0 } }
        else if ((c == "," || c == "}") && d == 0) { DONE = 1; REST = substr(s, i); return 0 }
        else if (c != " " && c != "\t") seen = 1
      }
      if (d == 0 && seen) DONE = 1
      return d
    }
    # value starts scanning a key value; col is the key column for YAML.
    function value(s, col) {
      DEPTH = scan(s, 0)
      if (DONE) return 1
      if (DEPTH > 0) MODE = "list"
      else { MODE = "pending"; COL = col }
      return 0
    }
    # keys finds every Action/Actions key on a line and scans its value.
    function keys(s,    t, m, lead, nx) {
      while (s != "") {
        t = "," tolower(s)
        if (!match(t, /[{,][ \t]*(-[ \t]+)?"?actions?"?[ \t]*[:=]/)) return
        nx = substr(s, RSTART + RLENGTH - 1, 1)
        if (nx == "=" || nx == ">" || nx == "~") { s = substr(s, RSTART + RLENGTH); continue }
        m = substr(t, RSTART + 1, RLENGTH - 1); lead = length(m)
        sub(/^[ \t]*(-[ \t]+)?/, "", m); lead -= length(m)
        if (!value(substr(s, RSTART + RLENGTH - 1), RSTART + lead - 1)) return
        s = REST
      }
    }
    { sub(/\r$/, "") }
    MODE == "list" { DEPTH = scan($0, DEPTH); if (DONE) { MODE = ""; keys(REST) }; next }
    /^[ \t]*$/ { next }
    MODE == "pending" {
      MODE = ""
      if ($0 ~ /^[ \t]*-([ \t]|$)/) MODE = "block"
      else if ($0 ~ /^[ \t]*[["\047]/) { if (value($0, COL)) keys(REST); next }
    }
    MODE == "block" {
      if ($0 ~ /^[ \t]*-([ \t]|$)/ && match($0, /[^ \t]/) - 1 >= COL) {
        item = $0; sub(/^[ \t]*-/, "", item); scan(item, 0); next
      }
      MODE = ""
    }
    { keys($0) }
    END { if (FOUND) print "*" }
  '
}

extract_actions() {
  local file="$1"
  local exclude_orgs="${2:-}"

  if [[ ! -f "$file" ]]; then
    echo "ERROR: file not found: $file" >&2
    exit 2
  fi

  local actions
  local stripped
  stripped=$(grep -vE '^[[:space:]]*(#|//)' "$file" || true)
  actions=$({
    printf '%s\n' "$stripped" \
      | grep -oE "(^|[^A-Za-z:])(${ACTION_PREFIXES}):([A-Z][A-Za-z]*\*?|\*)" \
      | sed 's/^[^a-zA-Z]//' || true
    printf '%s\n' "$stripped" | bare_star_action
  } | sort -u)

  if [[ "$exclude_orgs" == "no-orgs" ]]; then
    actions=$(printf '%s\n' "$actions" | grep -v '^organizations:' || true)
  fi

  if [[ -z "$actions" ]]; then
    echo "ERROR: no IAM actions extracted from: $file" >&2
    echo "       Did the policy move or change format?" >&2
    exit 2
  fi

  printf '%s\n' "$actions"
}

# --- comparison ---------------------------------------------------------------

FAILURES=0

compare_pair() {
  local label="$1" ref_name="$2" ref_actions="$3" other_name="$4" other_actions="$5"

  local diff_out
  diff_out=$(diff <(printf '%s\n' "$ref_actions") <(printf '%s\n' "$other_actions") || true)

  if [[ -n "$diff_out" ]]; then
    {
      echo "ERROR [$label]: IAM action drift between:"
      echo "  < $ref_name"
      echo "  > $other_name"
      echo "$diff_out"
      echo ""
    } >&2
    FAILURES=$((FAILURES + 1))
  fi
}

# --- 1. runtime: TF lambda vs TF fargate ---------------------------------------

TF_LAMBDA="$REPO_ROOT/terraform/modules/compute/aws/lambda/main.tf"
TF_FARGATE="$REPO_ROOT/terraform/modules/compute/aws/fargate/main.tf"

tf_lambda_actions=$(extract_actions "$TF_LAMBDA")
tf_fargate_actions=$(extract_actions "$TF_FARGATE")

compare_pair "runtime" "$TF_LAMBDA" "$tf_lambda_actions" "$TF_FARGATE" "$tf_fargate_actions"

# --- 2. federation cross-account pair (all namespaces) -------------------------

FED_XACC_CFN="$REPO_ROOT/iac/federation/aws-cross-account/cloudformation/template.yaml"
FED_XACC_TF="$REPO_ROOT/iac/federation/aws-cross-account/terraform/main.tf"

fed_cfn_actions=$(extract_actions "$FED_XACC_CFN")
fed_tf_actions=$(extract_actions "$FED_XACC_TF")

compare_pair "federation cross-account" "$FED_XACC_CFN" "$fed_cfn_actions" "$FED_XACC_TF" "$fed_tf_actions"

# --- 3. federation core across all flavors (org discovery excluded) ------------

FED_WIF_CFN="$REPO_ROOT/iac/federation/aws-target/cloudformation/template.yaml"
FED_WIF_TF="$REPO_ROOT/iac/federation/aws-target/terraform/main.tf"
FED_XACC_CLI="$REPO_ROOT/internal/iacfiles/templates/aws-cross-account-cli.sh.tmpl"
FED_WIF_CLI="$REPO_ROOT/internal/iacfiles/templates/aws-wif-cli.sh.tmpl"

fed_core_ref=$(extract_actions "$FED_XACC_CFN" no-orgs)

for f in "$FED_XACC_TF" "$FED_WIF_CFN" "$FED_WIF_TF" "$FED_XACC_CLI" "$FED_WIF_CLI"; do
  compare_pair "federation core" "$FED_XACC_CFN" "$fed_core_ref" "$f" "$(extract_actions "$f" no-orgs)"
done

# --- 4. required customer-account reads ----------------------------------------
# Parity only proves the files agree with each other, so a read the code calls
# in customer accounts that every flavor omits (#289) passes it. List those
# reads explicitly; each customer-facing policy and CLI template must grant all.

REQUIRED_CUSTOMER_ACTIONS=(
  ce:GetCostAndUsage
  ce:GetReservationPurchaseRecommendation ce:GetReservationUtilization ce:GetReservationCoverage
  ce:GetSavingsPlansPurchaseRecommendation ce:GetSavingsPlansUtilization ce:GetSavingsPlansCoverage
  ec2:DescribeInstanceTypeOfferings ec2:DescribeInstanceTypes
  ec2:GetReservedInstancesExchangeQuote savingsplans:DescribeSavingsPlansOfferings
)

for f in "$FED_XACC_CFN" "$FED_XACC_TF" "$FED_WIF_CFN" "$FED_WIF_TF" "$FED_XACC_CLI" "$FED_WIF_CLI" \
  "$REPO_ROOT/cloudformation/stacks/CUDly-CrossAccount/template.yaml" \
  "$TF_LAMBDA" "$TF_FARGATE"; do
  granted=$(extract_actions "$f")
  for a in "${REQUIRED_CUSTOMER_ACTIONS[@]}"; do
    if ! grep -qxF "$a" <<<"$granted"; then
      echo "ERROR [required customer actions]: $f does not grant $a" >&2
      FAILURES=$((FAILURES + 1))
    fi
  done
done

# --- 5. runtime organizations grants -------------------------------------------
# Parity only proves the runtime copies agree, so an unused organizations action
# added to all of them (#69: DescribeAccount, DescribeOrganization) passes it.
# The runtime calls only ListAccounts (internal/accounts/org_discovery.go and
# the cloud-commitments-go AWS provider), so that is the only organizations
# action it may grant. Wire up a new call before adding its action here.

ALLOWED_RUNTIME_ORG_ACTIONS=(organizations:ListAccounts)

for f in "$TF_LAMBDA" "$TF_FARGATE"; do
  while IFS= read -r a; do
    if [[ " ${ALLOWED_RUNTIME_ORG_ACTIONS[*]} " != *" $a "* ]]; then
      echo "ERROR [runtime organizations actions]: $f grants $a, which the runtime never calls" >&2
      FAILURES=$((FAILURES + 1))
    fi
  done < <(extract_actions "$f" | grep '^organizations:' || true)
done

# --- result --------------------------------------------------------------------

if [[ "$FAILURES" -gt 0 ]]; then
  echo "FAILED: $FAILURES IAM parity comparison(s) drifted. Update the lagging file(s) so all lists match." >&2
  exit 1
fi

echo "OK: AWS IAM action lists are in parity across CloudFormation, Terraform, and CLI onboarding templates."
exit 0
