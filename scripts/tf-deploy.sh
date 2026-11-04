#!/bin/bash
# Terraform Deployment Helper Script
# Usage: ./scripts/tf-deploy.sh <provider> <profile> [action] [extra terraform args...]
# Example: ./scripts/tf-deploy.sh aws dev
# Example: ./scripts/tf-deploy.sh aws prod plan
# Example: ./scripts/tf-deploy.sh aws dev apply -target=module.frontend
#
# Each provider is one flat Terraform root, terraform/environments/<provider>,
# the same root the deploy workflows init and apply. The profile picks the
# inputs, as the workflows do with -var-file and -backend-config:
#   variables: terraform/profiles/<provider>/<profile>.tfvars
#   backend:   terraform/environments/<provider>/backends/<profile>.tfbackend
# Both are gitignored; copy them from the .example files next to them.
#
# The root is shared by every profile, so each profile gets its own Terraform
# data directory (TF_DATA_DIR=<root>/.terraform/profiles/<profile>). That keeps
# one profile's backend from being used for another: with a single .terraform,
# `tf-deploy.sh aws prod` after `tf-deploy.sh aws dev` would apply the prod
# variables to the dev state.

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Functions
log_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

log_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

log_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

log_error() {
    echo -e "${RED}❌ $1${NC}"
}

# Parse arguments
PROVIDER=${1:-}
PROFILE=${2:-}
ACTION=${3:-apply}

# Any further arguments are forwarded verbatim to terraform (e.g. -target=...).
EXTRA_ARGS=()
if [ "$#" -gt 3 ]; then
    EXTRA_ARGS=("${@:4}")
fi

if [ -z "$PROVIDER" ] || [ -z "$PROFILE" ]; then
    log_error "Usage: $0 <provider> <profile> [action] [extra terraform args...]"
    echo ""
    echo "Examples:"
    echo "  $0 aws dev           # Deploy to AWS dev"
    echo "  $0 aws prod plan     # Plan AWS prod deployment"
    echo "  $0 azure dev         # Deploy to Azure dev"
    echo "  $0 gcp dev           # Deploy to GCP dev"
    echo ""
    echo "Available providers: aws, azure, gcp"
    echo "Available actions: plan, apply, destroy, show, refresh"
    exit 1
fi

# Both names become path components, so refuse anything that could leave the
# intended directories.
case "$PROVIDER" in
    aws|azure|gcp) ;;
    *)
        log_error "Unknown provider: ${PROVIDER} (expected aws, azure or gcp)"
        exit 1
        ;;
esac
if ! [[ "$PROFILE" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]]; then
    log_error "Invalid profile name: ${PROFILE} (letters, digits, '-' and '_' only)"
    exit 1
fi

# Paths
PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROFILE_FILE="${PROJECT_ROOT}/terraform/profiles/${PROVIDER}/${PROFILE}.tfvars"
ENV_DIR="${PROJECT_ROOT}/terraform/environments/${PROVIDER}"
BACKEND_FILE="${ENV_DIR}/backends/${PROFILE}.tfbackend"
DATA_DIR="${ENV_DIR}/.terraform/profiles/${PROFILE}"

# Validate profile exists
if [ ! -f "$PROFILE_FILE" ]; then
    log_error "Profile not found: $PROFILE_FILE"
    echo ""
    echo "Available ${PROVIDER} profiles:"
    found=0
    for f in "${PROJECT_ROOT}/terraform/profiles/${PROVIDER}/"*.tfvars; do
        [ -e "$f" ] || continue
        echo "  $(basename "$f" .tfvars)"
        found=1
    done
    [ "$found" -eq 1 ] || echo "  (none)"
    echo ""
    echo "Create a new profile:"
    echo "  cp terraform/profiles/${PROVIDER}/dev.tfvars.example terraform/profiles/${PROVIDER}/${PROFILE}.tfvars"
    exit 1
fi

# The root is part of the repository; never create one.
if [ ! -f "${ENV_DIR}/backend.tf" ]; then
    log_error "Terraform root not found: ${ENV_DIR} (no backend.tf)"
    exit 1
fi

# Validate backend config exists
if [ ! -f "$BACKEND_FILE" ]; then
    log_error "Backend config not found: $BACKEND_FILE"
    echo ""
    echo "Create it from an example and fill in your state bucket/account:"
    echo "  ls terraform/environments/${PROVIDER}/backends/*.tfbackend.example"
    echo "  cp terraform/environments/${PROVIDER}/backends/dev.tfbackend.example terraform/environments/${PROVIDER}/backends/${PROFILE}.tfbackend"
    if [ "$PROVIDER" = "aws" ]; then
        echo "or have scripts/init-backend.sh create the bucket and write backends/<env>.tfbackend."
    fi
    exit 1
fi

# Display configuration
echo ""
log_info "Terraform Deployment"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Provider:     ${PROVIDER}"
echo "Profile:      ${PROFILE}"
echo "Action:       ${ACTION}"
echo "Profile File: ${PROFILE_FILE}"
echo "Backend:      ${BACKEND_FILE}"
echo "Working Dir:  ${ENV_DIR}"
echo "Data Dir:     ${DATA_DIR}"
if [ -n "${AWS_PROFILE:-}" ]; then
    echo "AWS Profile:  ${AWS_PROFILE}"
elif [ -n "${TF_VAR_aws_profile:-}" ]; then
    echo "AWS Profile:  ${TF_VAR_aws_profile}"
else
    log_warning "AWS_PROFILE not set. Set it with: export AWS_PROFILE=your-profile"
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Change to environment directory
cd "$ENV_DIR"
export TF_DATA_DIR="$DATA_DIR"

# Initialize Terraform if needed
if [ ! -d "$TF_DATA_DIR" ]; then
    log_info "Initializing Terraform..."
    terraform init -backend-config="$BACKEND_FILE"
    log_success "Terraform initialized"
    echo ""
fi

# Execute Terraform command
log_info "Running terraform ${ACTION}..."
echo ""

# ${EXTRA_ARGS[@]+...} keeps the empty-array expansion safe under set -u
# on bash 3.2 (macOS default).
case $ACTION in
    plan)
        terraform plan -var-file="$PROFILE_FILE" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
    apply)
        terraform apply -var-file="$PROFILE_FILE" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
    destroy)
        log_warning "This will destroy all resources!"
        read -r -p "Are you sure? (type 'yes' to confirm): " confirm
        if [ "$confirm" = "yes" ]; then
            terraform destroy -var-file="$PROFILE_FILE" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        else
            log_info "Destroy cancelled"
            exit 0
        fi
        ;;
    show)
        terraform show ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
    refresh)
        terraform refresh -var-file="$PROFILE_FILE" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
    output)
        terraform output ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
    *)
        terraform "$ACTION" -var-file="$PROFILE_FILE" ${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}
        ;;
esac

# Show outputs after successful apply (set -e has already exited on a failed one)
if [ "$ACTION" = "apply" ]; then
    echo ""
    log_success "Deployment complete!"
    echo ""
    log_info "Deployment Outputs:"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    terraform output
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
fi

log_success "Done!"
