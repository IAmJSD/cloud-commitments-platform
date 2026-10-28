#!/usr/bin/env bash
# delete-owned-vpc-peering.sh
#
# Deletes the Service Networking peering a Terraform state owns, then removes
# it from that state. Nothing is removed from state unless the delete
# succeeded.
#
# Usage: delete-owned-vpc-peering.sh TERRAFORM_STATE_DIR GCP_PROJECT_ID
#
# Runs before `terraform destroy` on the GCP staging state, after
# delete-owned-cloud-sql-instance.sh: the peering cannot be removed while the
# Cloud SQL instance still uses it, and Terraform's own destroy of
# google_service_networking_connection fails on that "producer services still
# using this connection" ordering, so the workflow deletes it directly.
#
# The step this replaces (#330) ran
#
#   terraform state rm module.networking.google_service_networking_connection.private_vpc_connection 2>/dev/null || true
#   gcloud services vpc-peerings delete ... --network=cudly-staging-vpc ... 2>/dev/null || echo "..."
#
# which had two defects:
#
#   1. State was removed BEFORE the delete, and the delete's failure was
#      swallowed. Unlike the root-level Cloud SQL addresses #1971 corrected,
#      this address is module-qualified, so the state rm was real: a failed
#      delete left a live peering that no state tracked any more, and the
#      following `terraform destroy` then failed on google_compute_network
#      with nothing left to reconcile against. Here the delete runs first, its
#      exit status is not masked, and `terraform state rm` runs only after it
#      returned 0.
#   2. The network was the literal "cudly-staging-vpc" rather than the name the
#      state publishes. A renamed network would have turned the delete into a
#      "not found" that the `|| echo` reported as "already gone", while the
#      state rm still ran. The network is now read from the state's
#      `network_name` output (terraform/environments/gcp/outputs.tf ->
#      module.networking.network_name -> google_compute_network.main.name),
#      which is the network google_service_networking_connection
#      .private_vpc_connection is attached to
#      (terraform/modules/networking/gcp/main.tf).
#
# Whether there is anything to delete is decided by the state, not by gcloud:
# the peering is deleted only while this state still tracks it. That makes a
# re-run after a completed cleanup a no-op (the address is already gone from
# state), and a re-run after a FAILED delete a retry (the address is still
# there). The address is compared by exact equality against an unfiltered
# `terraform state list`, by scripts/select-owned-name.sh, rather than with
# `terraform state list ADDRESS`: that form exits 1 both when the address is
# absent and when the state cannot be read (measured on Terraform 1.15.8), so
# the two cannot be told apart without swallowing an error.
#
# `gcloud services vpc-peerings delete` waits for the operation unless `--async`
# is passed, so its exit status IS the deletion confirmation, the same way
# `gcloud sql instances delete` is in the Cloud SQL script. No `--async` here.
#
# Nothing is swallowed: no `2>/dev/null`, no `|| true`, no `|| echo`.
#
# Exit codes:
#   0  completed, including the "peering no longer in state" case, which is
#      what a re-run after a completed cleanup looks like
#   1  the peering is in state but the network it belongs to cannot be read
#      from a usable `network_name` output (state is left untouched)
#   2  usage error (wrong arity, a state directory that does not exist, or a
#      malformed project id)
#   *  anything terraform, jq or the selector fails with, unmasked; a failed
#      delete exits with gcloud's own status and leaves state untouched

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PEERING_ADDR="module.networking.google_service_networking_connection.private_vpc_connection"

if [[ $# -ne 2 ]]; then
  echo "usage: $(basename "$0") TERRAFORM_STATE_DIR GCP_PROJECT_ID" >&2
  exit 2
fi

STATE_DIR="$1"
PROJECT="$2"

if [[ ! -d "$STATE_DIR" ]]; then
  echo "error: terraform state directory '${STATE_DIR}' does not exist" >&2
  exit 2
fi

# Same project id shape check as delete-owned-cloud-sql-instance.sh: a
# whitespace-only or malformed value would otherwise reach `gcloud --project=`.
if ! printf '%s' "$PROJECT" | grep -Eq '^[a-z][a-z0-9-]{5,29}$'; then
  echo "error: GCP project id is unset or malformed ('${PROJECT}'); refusing to run VPC peering cleanup" >&2
  exit 2
fi

# Unfiltered on purpose: the equality test is the selector's. A failed
# `terraform state list` fails the pipeline under pipefail, and so the step.
IN_STATE="$(terraform -chdir="$STATE_DIR" state list \
  | "${SCRIPT_DIR}/select-owned-name.sh" "$PEERING_ADDR")"

if [[ -z "$IN_STATE" ]]; then
  echo "State no longer tracks ${PEERING_ADDR}; nothing to delete, state left untouched."
  exit 0
fi

OUTPUTS_JSON="$(terraform -chdir="$STATE_DIR" output -json)"
# On its own line so a non-JSON `terraform output` fails here under `set -e`,
# loudly, instead of reading as an absent output below.
OUTPUTS_TYPE="$(jq -r 'type' <<<"$OUTPUTS_JSON")"
if [[ "$OUTPUTS_TYPE" != "object" ]]; then
  echo "error: 'terraform output -json' on '${STATE_DIR}' returned a ${OUTPUTS_TYPE}, not an object" >&2
  exit 1
fi

if ! NETWORK="$(jq -er '.network_name.value' <<<"$OUTPUTS_JSON")"; then
  echo "error: state '${STATE_DIR}' tracks ${PEERING_ADDR}, but its" >&2
  echo "       'network_name' output is absent or null, so the network the peering" >&2
  echo "       belongs to cannot be identified. Refusing to fall back to a hardcoded" >&2
  echo "       network name. State left untouched." >&2
  exit 1
fi

# RFC 1035, which is what google_compute_network accepts. An empty or
# whitespace-bearing value is a broken output, not a network.
if ! printf '%s' "$NETWORK" | grep -Eq '^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$'; then
  echo "error: state '${STATE_DIR}' publishes 'network_name' as '${NETWORK}', which" >&2
  echo "       is not a VPC network name. Inspect" >&2
  echo "       'terraform -chdir=${STATE_DIR} output -json' before destroying anything." >&2
  echo "       State left untouched." >&2
  exit 1
fi

echo "Deleting the Service Networking peering on network '${NETWORK}'..."
if gcloud services vpc-peerings delete \
  --service=servicenetworking.googleapis.com \
  --network="$NETWORK" \
  --project="$PROJECT" --quiet; then
  echo "Service Networking peering on network '${NETWORK}' deleted."
else
  rc=$?
  echo "error: deleting the Service Networking peering on network '${NETWORK}' failed" >&2
  echo "       (exit ${rc}); ${PEERING_ADDR} is left in state so a re-run retries it." >&2
  echo "       If the peering is confirmed gone already, remove the address by hand:" >&2
  echo "       terraform -chdir=${STATE_DIR} state rm ${PEERING_ADDR}" >&2
  exit "$rc"
fi

# Only after the delete above returned 0.
terraform -chdir="$STATE_DIR" state rm "$PEERING_ADDR"
