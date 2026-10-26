# Terraform Profiles

This directory contains deployment profiles for different
environments and cloud providers.

## What Are Profiles?

Profiles are pre-configured Terraform variable files (`.tfvars`)
that define environment-specific settings. Think of them as
deployment presets.

## Directory Structure

```text
profiles/
├── aws/
│   ├── dev.tfvars.example          # AWS Lambda development environment
│   ├── fargate-dev.tfvars.example  # AWS Fargate development environment
│   └── prod.tfvars.example         # AWS production environment
├── azure/
│   └── dev.tfvars.example          # Azure development environment
└── gcp/
    └── dev.tfvars.example          # GCP development environment
```

Only the `.tfvars.example` files are tracked. Copy one to a
`.tfvars` file (for example `aws/dev.tfvars`) before using it;
`*.tfvars` is gitignored.

Every variable a profile sets must be declared in the matching
`terraform/environments/<provider>/variables.tf`. Terraform only
warns about an undeclared variable in a `.tfvars` file and then
ignores it, so a misspelled or removed variable would silently
have no effect. `profile_vars_guard_test.go` in this directory
checks the examples and the snippets in this README against
those declarations (`go test ./terraform/profiles/`).

## Using Profiles

### Quick Deployment with Profile

```bash
# Deploy to AWS dev
cd terraform/environments/aws/dev
terraform apply -var-file="../../../profiles/aws/dev.tfvars"

# Deploy to AWS prod
cd terraform/environments/aws/prod
terraform apply -var-file="../../../profiles/aws/prod.tfvars"
```

### With Helper Script (Even Easier)

```bash
# From project root
./scripts/tf-deploy.sh aws dev       # Deploy to AWS dev
./scripts/tf-deploy.sh aws prod      # Deploy to AWS prod
./scripts/tf-deploy.sh azure dev     # Deploy to Azure dev
./scripts/tf-deploy.sh gcp dev       # Deploy to GCP dev

# Plan only (dry run)
./scripts/tf-deploy.sh aws dev plan
```

## Creating a New Profile

### Copy from Example

```bash
# Copy example profile
cp profiles/aws/dev.tfvars.example profiles/aws/my-profile.tfvars

# Edit with your settings
vim profiles/aws/my-profile.tfvars

# Use it
terraform apply -var-file="../../../profiles/aws/my-profile.tfvars"
```

## Profile Contents

Each profile contains environment-specific variables. An excerpt
of `aws/dev.tfvars.example`:

```hcl
# profiles/aws/dev.tfvars

# Project identification
project_name = "cudly"
environment  = "dev"

# Cloud configuration
region = "us-east-1"
# aws_profile = "your-aws-profile"

# Compute platform
compute_platform = "lambda"

# Lambda configuration
lambda_architecture = "arm64"
lambda_memory_size  = 512

# Docker build settings
enable_docker_build = true

# Database (RDS PostgreSQL)
database_name           = "cudly"
database_engine_version = "16" # major-only pin; see variables.tf

# Networking
vpc_cidr = "10.0.0.0/16"

# Frontend
enable_cdn = false
```

The provider is chosen by the environment directory you run
Terraform in (`terraform/environments/<provider>`), not by a
variable.

`enable_docker_build` is the only build switch a profile can set.
With `true` (the AWS default; Azure and GCP default to `false`),
`terraform apply` builds the container image and pushes it to the
registry. With `false`, nothing is built and the deployment uses
the pre-built image in `image_uri`.
There is no option to build without pushing, and no separate
frontend build switch: the frontend is compiled into the same
container image by the Dockerfile's `frontend-builder` stage.
`skip_docker_build`, `skip_docker_push` and `enable_frontend_build`
are not root variables, so setting them in a profile does nothing.

## Common Patterns

### Development Profile

```hcl
# profiles/aws/dev.tfvars
environment                    = "dev"
lambda_memory_size             = 512   # Minimum resources
az_count                       = 2
enable_flow_logs               = false # Less auditing
enable_monitoring              = false # No alarms
database_deletion_protection   = false
database_skip_final_snapshot   = true
database_backup_retention_days = 7
```

### Staging Profile

There is no staging example; a staging profile usually sits
between the dev and prod examples:

```hcl
# profiles/aws/staging.tfvars
environment                    = "staging"
lambda_memory_size             = 1024 # More resources
az_count                       = 2
enable_flow_logs               = true # Full auditing
enable_monitoring              = true # Alarms and alert emails
database_deletion_protection   = false
database_skip_final_snapshot   = false
database_backup_retention_days = 7
```

### Production Profile

```hcl
# profiles/aws/prod.tfvars
environment                    = "prod"
lambda_memory_size             = 1024 # More resources
az_count                       = 3    # More AZs
enable_flow_logs               = true
database_performance_insights  = true
enable_monitoring              = true # prod warns without it; see below
database_deletion_protection   = true
database_skip_final_snapshot   = false
database_backup_retention_days = 30
```

`enable_monitoring` (default `false` on all three providers)
creates the monitoring module: alarms and alert notifications
(on Azure only for Container Apps, on GCP only for Cloud Run).
Alerts go to `alert_emails`, a comma-separated list. Don't commit
real addresses in a profile: the deploy workflow passes them from
the `ALERT_EMAILS` secret, and for a local run you can
`export TF_VAR_alert_emails="ops@example.com"`. With
`environment = "prod"`, each root prints a
`prod_alerts_reach_someone` warning if monitoring is off or no
address is set.

## Multi-Cloud Profiles

### AWS Lambda (Serverless)

```hcl
# profiles/aws/dev.tfvars
compute_platform    = "lambda"
region              = "us-east-1"
lambda_architecture = "arm64"
lambda_memory_size  = 512
```

### AWS Fargate (Container)

```hcl
# profiles/aws/fargate-dev.tfvars
compute_platform = "fargate"
region           = "us-east-1"
fargate_cpu      = 512
fargate_memory   = 1024
```

### Azure Container Apps

```hcl
# profiles/azure/dev.tfvars
compute_platform = "container-apps"
location         = "eastus"
container_cpu    = 0.5
container_memory = "1.0Gi"
```

### GCP Cloud Run

```hcl
# profiles/gcp/dev.tfvars
compute_platform = "cloud-run"
region           = "us-central1"
cloud_run_cpu    = "1"
cloud_run_memory = "512Mi"
```

## Secrets Management

**Never commit secrets to profiles!**

### Using Environment Variables

```bash
# Set secrets as environment variables
export TF_VAR_admin_password="secret123"
export TF_VAR_credential_encryption_key="$(openssl rand -hex 32)"

# Terraform automatically picks them up
terraform apply -var-file="../../../profiles/aws/dev.tfvars"
```

### Using secrets.tfvars (Gitignored)

```bash
# Create secrets file (not tracked in git)
cat > profiles/aws/secrets.tfvars <<EOF
admin_password            = "secret123"
credential_encryption_key = "<64-char hex string>"
EOF

# Use both profile and secrets
terraform apply \
  -var-file="../../../profiles/aws/dev.tfvars" \
  -var-file="../../../profiles/aws/secrets.tfvars"
```

> **Note:** `admin_password` is optional. Without it,
> after deployment, the admin account is bootstrapped through
> the UI using an API key (stored in AWS Secrets Manager)
> and the `/auth/setup-admin` endpoint. Alternatively, the
> admin is created with the `ADMIN_EMAIL` env var during
> database migration with no password set, requiring a
> "Forgot Password" flow to set the initial password.

### Generated Secrets

The database password, JWT and session secrets are generated by
Terraform and stored in the cloud's secret manager (AWS Secrets
Manager, Azure Key Vault or GCP Secret Manager). A profile does
not need to reference them.

## Profile Validation

Validate profile before applying:

```bash
# Check syntax
terraform validate

# Dry run (plan)
terraform plan -var-file="../../../profiles/aws/dev.tfvars"
```

Treat any `Value for undeclared variable` warning from `plan` as
an error: Terraform is ignoring that line of the profile.

## Best Practices

### 1. Use Consistent Naming

```text
{provider}/{environment}.tfvars
  aws/dev.tfvars
  aws/staging.tfvars
  aws/prod.tfvars
```

### 2. Document Custom Values

```hcl
# profiles/aws/dev.tfvars

# Using Graviton (ARM) for cost savings
lambda_architecture = "arm64"

# Reduced capacity for dev environment
lambda_memory_size = 512 # vs 1024 in prod
```

### 3. Version Control Profiles

```bash
# Commit profiles (without secrets)
git add profiles/aws/dev.tfvars
git add profiles/aws/prod.tfvars

# Ignore secrets
echo "profiles/**/secrets.tfvars" >> .gitignore
```

### 4. Use Profile for Each Environment

```text
profiles/
├── aws/
│   ├── dev.tfvars        ← John's development
│   ├── dev-jane.tfvars   ← Jane's development
│   ├── staging.tfvars    ← Shared staging
│   └── prod.tfvars       ← Production
```

## Migrating from CUDly CLI Profiles

Convert `~/.cudly/deployment.yaml` to Terraform profile:

```bash
# CLI profile
cat ~/.cudly/deployment.yaml
# active_profile: aws-dev
# profiles:
#   aws-dev:
#     provider: aws
#     compute_platform: lambda
#     region: us-east-1

# Convert to Terraform profile (the provider is the
# terraform/environments/<provider> directory, not a variable)
cat > profiles/aws/dev.tfvars <<EOF
compute_platform = "lambda"
region           = "us-east-1"
environment      = "dev"
project_name     = "cudly"
EOF
```

## Helper Scripts

### tf-deploy.sh

```bash
#!/bin/bash
# Deploy using profile

PROVIDER=$1
PROFILE=$2
ACTION=${3:-apply}

PROFILE_FILE="profiles/${PROVIDER}/${PROFILE}.tfvars"

if [ ! -f "$PROFILE_FILE" ]; then
  echo "Error: Profile not found: $PROFILE_FILE"
  exit 1
fi

cd "terraform/environments/${PROVIDER}/${PROFILE}"

terraform init
terraform $ACTION -var-file="../../../../${PROFILE_FILE}"
```

## Related Documentation

- [Docker Build Module](../modules/build/README.md)
- [Environment Setup](../environments/README.md)
- [Deployment Guide](../../docs/DEPLOYMENT.md)
