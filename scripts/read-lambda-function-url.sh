#!/usr/bin/env bash
# read-lambda-function-url.sh
#
# Prints the Lambda Function URL recorded by the previous apply of the AWS
# environment, so a deploy can hand it back to Terraform as
# TF_VAR_lambda_deployed_function_url and keep that origin in the Function
# URL's CORS allowlist (#324). The URL ID is server-assigned and only known
# after an apply, which is why it is read from state instead of being pasted
# into github-<env>.tfvars by hand.
#
# Usage: scripts/read-lambda-function-url.sh <terraform-dir>
#
# The directory must already be initialised against its backend. Prints:
#   - the URL with any trailing slash removed, when state has one;
#   - nothing (exit 0) when state has no lambda_function_url value yet, i.e.
#     the first deploy, or a Fargate stack whose output is null.
# Exits 1 when state holds something that is not a Lambda Function URL, so a
# corrupted or unexpected value stops the deploy instead of reaching CORS.
#
# Read-only: runs `terraform output` and nothing else.

set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 <terraform-dir>" >&2
  exit 2
fi

# `terraform output -raw` fails when the output is missing (no state yet) or
# null, and both mean "no deployed URL". hashicorp/setup-terraform's wrapper
# adds ::debug:: lines to stdout, which are dropped the same way the deploy
# workflow's "Get Terraform outputs" step drops them.
raw=$(terraform -chdir="$1" output -raw lambda_function_url 2>/dev/null | grep -v '::' || true)

if [[ -z "$raw" ]]; then
  exit 0
fi

url_re='^https://[a-z0-9]+\.lambda-url\.[a-z0-9-]+\.on\.aws/?$'
if [[ "$raw" == *$'\n'* || ! "$raw" =~ $url_re ]]; then
  echo "ERROR: lambda_function_url in state is not a Lambda Function URL; refusing to pass it to the CORS allowlist" >&2
  exit 1
fi

printf '%s\n' "${raw%/}"
