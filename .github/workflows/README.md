# GitHub Actions Workflows

This directory contains CI/CD workflows for the CUDly project, providing automated testing, deployment, and operations across AWS, GCP, and Azure.

## 📋 Workflows Overview

| Workflow | Purpose | Trigger | Duration |
|----------|---------|---------|----------|
| [ci.yml](#ci-workflow) | Continuous Integration | PR, Push to main | ~10 min |
| [deploy-aws-lambda.yml](#aws-lambda-deployment) | Deploy to AWS Lambda | Push to main, Manual | ~8 min |
| [deploy-aws-fargate.yml](#aws-fargate-deployment) | Deploy to AWS Fargate | Manual | ~10 min |
| [deploy-gcp.yml](#gcp-deployment) | Deploy to GCP Cloud Run | Manual | ~8 min |
| [deploy-azure.yml](#azure-deployment) | Deploy to Azure Container Apps | Manual | ~10 min |
| [deploy-all.yml](#multi-cloud-deployment) | Deploy to all clouds | Manual, Release | ~15 min |
| [database-migration.yml](#database-migrations) | Run DB migrations | Manual | ~5 min |
| [rollback.yml](#rollback) | Rollback deployment | Manual | ~5 min |

> **Note:** Frontend deployment is handled automatically via Terraform as part of the backend deployment workflows.

---

## CI Workflow

**File:** `ci.yml`

### Purpose

Runs comprehensive quality checks on every pull request and push to main branch.

### Jobs

1. **Lint** - golangci-lint, go vet
2. **Unit Tests** - Go tests with race detection, coverage reporting
3. **Integration Tests** - Tests with real PostgreSQL: `integration`-tagged tests and tests that skip under `-short`, sharded across parallel runners (selected by `scripts/integration-test-selection.sh`)
4. **Docker Build** - Build, check and scan the Docker image natively for linux/amd64 and linux/arm64
5. **Terraform Validate** - Validate all Terraform configs (AWS, GCP, Azure)
6. **Security Scan** - gosec, trivy, tfsec
7. **Snyk Scan** - Dependency vulnerability scanning
8. **E2E Tests** - Docker Compose end-to-end tests
9. **Cost Estimate** - Infracost cost estimation (PR only)

### Triggers

- Pull requests to `main` or `develop`
- Pushes to `main` or `develop`
- Manual dispatch

### Required Secrets

- `SNYK_TOKEN` (optional - for Snyk scanning)
- `INFRACOST_API_KEY` (optional - for cost estimation)

### Required Variables

- `GO_VERSION` (default: 1.26.10)

### Example

```bash
# Automatically runs on PR
git push origin feature-branch

# Or trigger manually
gh workflow run ci.yml
```

---

## AWS Lambda Deployment

**File:** `deploy-aws-lambda.yml`

### Purpose

Deploy CUDly to AWS Lambda with Function URL. Serverless, event-driven platform.

### Jobs

1. **Prepare** - Determine environment and image tag
2. **Build & Push** - Build Docker image, push to ECR
3. **Deploy** - Deploy with Terraform
4. **Test** - Health check and smoke tests

### Triggers

- Push to `main` (deploys to dev)
- Release creation (deploys to prod)
- Manual dispatch with environment selection

### Required Secrets

- `ADMIN_EMAIL`
- `TF_BACKEND_AWS`
- `ALERT_EMAILS`, `FROM_EMAIL`, `DASHBOARD_URL` (optional)

No AWS access keys are stored. `configure-aws-credentials` assumes `AWS_ROLE_TO_ASSUME` with the
job's GitHub OIDC token (see the AWS part of the [Setup Guide](#setup-guide)).

### Required Variables

- `AWS_ROLE_TO_ASSUME` (ARN of the deploy role)
- `AWS_REGION` (default: us-east-1)
- `AWS_ACCOUNT_ID`
- `ECR_REPOSITORY` (default: cudly)

### Example

```bash
# Deploy to dev
gh workflow run deploy-aws-lambda.yml -f environment=dev

# Push to main also deploys to dev
git push origin main

# Deploy to prod
gh release create v1.0.0
```

### Output

- Function URL: `https://<id>.lambda-url.us-east-1.on.aws`
- Deployment info artifact

---

## AWS Fargate Deployment

**File:** `deploy-aws-fargate.yml`

### Purpose

Deploy CUDly to AWS ECS Fargate with ALB. Always-on containerized platform.

### Jobs

1. **Build & Push** - Build Docker image, push to ECR
2. **Deploy** - Deploy with Terraform (Fargate mode)
3. **Test** - Health check verification

### Triggers

- Manual dispatch only

### Required Secrets

- Same as AWS Lambda

### Example

```bash
# Deploy to staging with Fargate
gh workflow run deploy-aws-fargate.yml -f environment=staging
```

---

## GCP Deployment

**File:** `deploy-gcp.yml`

### Purpose

Deploy CUDly to GCP Cloud Run. Serverless container platform.

### Jobs

1. **Build & Deploy** - Build, push to Artifact Registry, deploy with Terraform
2. **Test** - Health check and smoke tests

### Triggers

- Manual dispatch
- Called by deploy-all.yml

### Required Secrets

- `ADMIN_EMAIL`
- `TF_BACKEND_GCP`
- `ALERT_EMAILS` (optional)

No service-account key is stored. `google-github-actions/auth` exchanges the job's GitHub OIDC
token through Workload Identity Federation (see the GCP part of the [Setup Guide](#setup-guide)).

### Required Variables

- `GCP_WORKLOAD_IDENTITY_PROVIDER` (full resource name of the Workload Identity provider)
- `GCP_SERVICE_ACCOUNT` (email of the deploy service account)
- `GCP_PROJECT_ID`
- `GCP_REGION` (default: us-central1)
- `ARTIFACT_REGISTRY_REPO` (default: cudly)

### Example

```bash
# Deploy to GCP dev
gh workflow run deploy-gcp.yml -f environment=dev
```

### Output

- Service URL: `https://cudly-<hash>-uc.a.run.app`

---

## Azure Deployment

**File:** `deploy-azure.yml`

### Purpose

Deploy CUDly to Azure Container Apps. Serverless container platform with built-in HTTPS.

### Jobs

1. **Build & Deploy** - Build, push to ACR, deploy with Terraform
2. **Test** - Health check and smoke tests

### Triggers

- Push to `main` (deploys to dev)
- Manual dispatch
- Called by deploy-all.yml

### Required Secrets

- `AZURE_CLIENT_ID` (client ID of the deploy app registration)
- `AZURE_TENANT_ID`
- `AZURE_SUBSCRIPTION_ID`
- `ADMIN_EMAIL`
- `TF_BACKEND_AZURE`
- `ALERT_EMAILS` (optional)

No client secret or service-principal JSON is stored. `azure/login` swaps the job's GitHub OIDC
token for a short-lived Azure token through a federated identity credential (see the Azure part of
the [Setup Guide](#setup-guide)).

### Required Variables

- `AZURE_LOCATION` (default: westus2)
- `ACR_NAME` (default: cudlyacr)

### Example

```bash
# Deploy to Azure staging
gh workflow run deploy-azure.yml -f environment=staging
```

### Output

- App URL: `https://<app-name>.<region>.azurecontainerapps.io`

---

## Multi-Cloud Deployment

**File:** `deploy-all.yml`

### Purpose

Orchestrate deployment to multiple cloud providers in parallel.

### Jobs

1. **Determine Strategy** - Choose which clouds to deploy to
2. **Deploy AWS Lambda** - Parallel deployment
3. **Deploy AWS Fargate** - Parallel deployment (optional)
4. **Deploy GCP** - Parallel deployment
5. **Deploy Azure** - Parallel deployment
6. **Notify** - Aggregate results

### Triggers

- Manual dispatch with provider selection
- Release creation (deploys to all clouds in prod)

### Required Secrets

- All secrets from individual deployment workflows

### Deployment Options

- `all` - Deploy to AWS, GCP, and Azure
- `aws-only` - AWS Lambda only
- `gcp-only` - GCP Cloud Run only
- `azure-only` - Azure Container Apps only
- `aws-gcp` - AWS and GCP
- `aws-azure` - AWS and Azure
- `gcp-azure` - GCP and Azure

### Example

```bash
# Deploy to all clouds (staging)
gh workflow run deploy-all.yml -f environment=staging -f deploy_to=all

# Deploy to AWS and GCP (prod)
gh workflow run deploy-all.yml -f environment=prod -f deploy_to=aws-gcp

# Automatic on release
gh release create v1.0.0
```

### Benefits

- **Disaster Recovery** - Multi-cloud redundancy
- **Cost Optimization** - Compare costs across providers
- **Testing** - Validate across all platforms
- **Global Reach** - Deploy to optimal regions per cloud

---

## Database Migrations

**File:** `database-migration.yml`

### Purpose

Apply or rollback database schema migrations across cloud providers.

### Jobs

1. **Validate** - Safety checks
2. **Migrate AWS** - Run golang-migrate on Aurora
3. **Migrate GCP** - Run golang-migrate on Cloud SQL
4. **Migrate Azure** - Run golang-migrate on Flexible Server

### Triggers

- Manual dispatch only (safety measure)
- Can be called by deployment workflows

### Required Secrets

- `DB_PASSWORD_AWS`
- `DB_PASSWORD_GCP`
- `DB_PASSWORD_AZURE`
- Cloud credentials (same as deployment workflows)

### Required Variables

- Database endpoints per environment

### Migration Directions

- `up` - Apply migrations (default)
- `down` - Rollback migrations (DANGEROUS)

### Example

```bash
# Apply all migrations to AWS dev
gh workflow run database-migration.yml \
  -f cloud=aws \
  -f environment=dev \
  -f direction=up

# Rollback last 2 migrations on GCP staging
gh workflow run database-migration.yml \
  -f cloud=gcp \
  -f environment=staging \
  -f direction=down \
  -f steps=2

# Rollback 1 migration on AWS prod (the confirmation repeats the step count)
gh workflow run database-migration.yml \
  -f cloud=aws \
  -f environment=prod \
  -f direction=down \
  -f steps=1 \
  -f confirm=rollback-prod-1

# Apply to all clouds
gh workflow run database-migration.yml \
  -f cloud=all \
  -f environment=prod \
  -f direction=up
```

### Safety Features

- **Validation** - Checks migration files exist before running
- **Explicit steps required** - `direction=down` requires an explicit positive `steps` value; `steps=0` (the default, which would run `down -all` and drop the entire schema) is rejected
- **Bounded steps** - `direction=down` also rejects any `steps` at or above the number of migrations in the tree, in every environment. golang-migrate does not refuse a `down N` larger than what is applied: it rolls back everything that remains and only then reports a short limit, so `steps=99999999` is `down -all` (#68)
- **Production confirmation** - `direction=down` on `environment=prod` additionally requires typing `rollback-prod-<steps>` (for example `rollback-prod-1` with `steps=1`) in the `confirm` input, so the confirmation restates how many migrations are being dropped; omitting it, mistyping it, or a count that differs from `steps` blocks the run
- **Defense in depth** - each migrate job runs the same gate (`scripts/check-migration-down-request.sh`) again right before calling migrate, so a future validate regression cannot reach `down -all`
- **Audit Trail** - Records all migrations in the step summary

---

## Rollback

**File:** `rollback.yml`

### Purpose

Quickly rollback to a previous deployment version by redeploying a known-good Docker image.

### Jobs

1. **Validate** - Validate image tag and construct image URI
2. **Rollback** - Confirm the image exists in the registry, then deploy it with Terraform
3. **Summary** - Create audit record

Image existence is verified *inside* each rollback job rather than in a
standalone job. A separate verify job would have to assume the same cloud
deploy role while carrying no `environment:` binding, which is exactly the
ungated-but-credentialed shape that made the workflow exploitable. The
tradeoff is that a rollback to a nonexistent tag now fails after the
environment approval rather than before it.

### Triggers

- Manual dispatch only (safety measure)

### Required Secrets

- Cloud credentials (same as deployment workflows)

### Example

```bash
# Rollback AWS Lambda production to previous version
gh workflow run rollback.yml \
  -f cloud=aws-lambda \
  -f environment=prod \
  -f image_tag=sha-abc123 \
  -f reason="Critical bug in v1.2.3"

# Rollback GCP staging
gh workflow run rollback.yml \
  -f cloud=gcp \
  -f environment=staging \
  -f image_tag=v1.2.2
```

### Safety Features

- **Image Verification** - Confirms image exists before deploying. For AWS,
  the check parses the account, region and repository out of the exact image
  URI Terraform deploys, refuses to run if that account is not the account of
  `AWS_ROLE_TO_ASSUME`, and pins the lookup to that registry
- **Audit Trail** - Records all rollbacks (365 day retention)
- **Reason Tracking** - Requires reason for accountability
- **Manual Only** - Cannot be triggered automatically

### Finding Image Tags

```bash
# AWS ECR
aws ecr list-images --repository-name cudly

# GCP Artifact Registry
gcloud artifacts docker images list <region>-docker.pkg.dev/<project>/<repo>/cudly

# Azure ACR
az acr repository show-tags --name cudlyacr --repository cudly
```

---

## Setup Guide

### 1. Configure GitHub Secrets

No cloud uses a long-lived key. Each workflow exchanges the job's GitHub OIDC token (issuer
`https://token.actions.githubusercontent.com`) for short-lived cloud credentials. The cloud-side
trust and deploy identities come from the bootstrap modules in
`terraform/environments/{aws,gcp,azure}/ci-cd-permissions/`, which a privileged operator applies by
hand; no workflow applies them.

This repository has GitHub's immutable OIDC subject enabled, so every `sub` claim starts with
`repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743`, not
`repo:LeanerCloud/cloud-commitments-platform`. Check the current prefix with
`gh api repos/LeanerCloud/cloud-commitments-platform/actions/oidc/customization/sub`. The AWS and
Azure modules build each subject as `repo:<github_repo>:...`, so their `github_repo` input must be
that `owner@id/repo@id` form; the plain-name default produces subjects no token presents. GCP does
not match on `sub` and is unaffected.

**AWS:**

The bootstrap module creates the GitHub OIDC provider and the `cudly-terraform-deploy` role. Every
`aws-actions/configure-aws-credentials` step reads the role ARN from a repository variable:

```bash
# Run from terraform/environments/aws/ci-cd-permissions/ after it has been applied
gh variable set AWS_ROLE_TO_ASSUME -b"$(terraform output -raw role_arn)"
gh secret set DB_PASSWORD_AWS
```

The role's trust policy accepts:

| Field | Value |
| --- | --- |
| Issuer (OIDC provider URL) | `https://token.actions.githubusercontent.com` |
| Audience (`aud`) | `sts.amazonaws.com` |
| Subject (`sub`) | exact match (`StringEquals`) against the literal list in `role.tf` |

Every job that assumes the role is bound to a GitHub Environment, so it presents an
`environment:<name>` subject:

| Environment | Jobs that present it |
| --- | --- |
| `dev`, `staging`, `prod` | `deploy-aws-lambda.yml` (`build-and-deploy`, `test-deployment`), `rollback.yml` (`rollback-aws-lambda`), `cleanup-staging.yml` (`destroy-aws-lambda`, `destroy-aws-fargate`, `staging` only), `destroy-fargate-dev.yml` (`destroy`, `dev` only) |
| `aws-fargate-dev`, `aws-fargate-staging`, `aws-fargate-prod` | `deploy-aws-fargate.yml` (`deploy`), `rollback.yml` (`rollback-aws-fargate`) |
| `aws-db-dev`, `aws-db-staging`, `aws-db-prod` | `database-migration.yml` (`migrate-aws`) |

`role.tf` lists all of these, plus `ref:refs/heads/main`, which no current AWS job presents.

**GCP:**

The bootstrap module creates the `cudly-terraform-deploy` service account, a Workload Identity pool
and provider, and the `roles/iam.workloadIdentityUser` binding that lets this repository's tokens
impersonate the service account. Every `google-github-actions/auth` step reads two repository
variables:

```bash
# Run from terraform/environments/gcp/ci-cd-permissions/ after it has been applied
gh variable set GCP_WORKLOAD_IDENTITY_PROVIDER -b"$(terraform output -raw workload_identity_provider)"
gh variable set GCP_SERVICE_ACCOUNT -b"$(terraform output -raw service_account_email)"
gh variable set GCP_PROJECT_ID -b"<project-id>"
gh secret set DB_PASSWORD_GCP
```

| Field | Value |
| --- | --- |
| Issuer | `https://token.actions.githubusercontent.com` |
| Audience | the provider's default (no `allowed_audiences` set), which `google-github-actions/auth` requests by default |
| Attribute condition | `repository_owner_id` and `repository_id` equal the module's `github_owner_id` / `github_repo_id`, and `ref` equals `deploy_ref` (default `refs/heads/main`) |

The condition ignores `sub`, so the GCP jobs (`deploy-gcp.yml` `build-and-deploy`, `rollback.yml`
`rollback-gcp`, `cleanup-staging.yml` `destroy-gcp`, `database-migration.yml` `migrate-gcp`)
authenticate only when the run's ref is `deploy_ref`, whatever environment they bind.

The deploy service account's project roles, `roles/run.admin` among them, are the
`local.deploy_roles` list in the module's `service_account.tf` (`terraform output roles` prints
them). Change that list and re-apply the module instead of granting roles by hand.

**Azure:**

Azure authenticates with OIDC, so no client secret or `--sdk-auth` JSON is created or stored. The
deploy app registration, its service principal and its federated identity credentials come from the
bootstrap module `terraform/environments/azure/ci-cd-permissions/`, which a privileged operator
applies by hand (see its [README](../../terraform/environments/azure/ci-cd-permissions/README.md)).
Every `azure/login` step reads the same three secrets:

```bash
# Run from terraform/environments/azure/ci-cd-permissions/ after it has been applied
gh secret set AZURE_CLIENT_ID -b"$(terraform output -raw client_id)"
gh secret set AZURE_TENANT_ID -b"$(terraform output -raw tenant_id)"
gh secret set AZURE_SUBSCRIPTION_ID -b"<subscription-id>"
gh secret set DB_PASSWORD_AZURE
```

Each federated identity credential on the app registration must use:

| Field | Value |
| --- | --- |
| Issuer | `https://token.actions.githubusercontent.com` |
| Audience | `api://AzureADTokenExchange` |
| Subject | `repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:<name>` (see the immutable-subject note above) |

Every job that runs `azure/login` is bound to a GitHub Environment, so its token carries the
`environment:<name>` subject, not a branch subject, even when it runs on `main`. Azure allows no
wildcards, so each environment name needs its own credential:

| Environment | Jobs that present it |
| --- | --- |
| `dev`, `staging`, `prod` | `deploy-azure.yml` (`build-and-deploy`, `test-deployment`), `rollback.yml` (`rollback-azure`), `cleanup-staging.yml` (`destroy-azure`, `staging` only) |
| `azure-db-dev`, `azure-db-staging`, `azure-db-prod` | `database-migration.yml` (`migrate-azure`) |

The bootstrap module creates one credential per name in its `github_environments` variable
(default `dev`, `staging`, `prod`). It does not yet create the `azure-db-*` credentials, so
`migrate-azure` fails with `AADSTS70021` until those environments exist with protection rules and
are added to `github_environments`. The module also creates a `ref:refs/heads/main` credential; no
current Azure job presents that subject. The environment subject does not encode the branch, so
restrict branches with each environment's deployment branch policy (step 3 below).

**Optional:**

```bash
gh secret set SNYK_TOKEN
gh secret set INFRACOST_API_KEY
```

### 2. Configure GitHub Variables

```bash
# AWS
gh variable set AWS_REGION -b"us-east-1"
gh variable set AWS_ACCOUNT_ID -b"123456789012"
gh variable set ECR_REPOSITORY -b"cudly"

# GCP
gh variable set GCP_REGION -b"us-central1"
gh variable set ARTIFACT_REGISTRY_REPO -b"cudly"

# Azure
gh variable set AZURE_LOCATION -b"eastus"
gh variable set ACR_NAME -b"cudlyacr"

# Frontend
gh variable set CLOUD_PROVIDER -b"aws"
gh variable set FRONTEND_BUCKET -b"cudly-frontend-prod"
gh variable set CLOUDFRONT_DISTRIBUTION_ID -b"E1234567890"
gh variable set API_URL -b"https://api.cudly.example.com"
```

### 3. Set Up Environments

GitHub Environments provide deployment protection and environment-specific secrets.
GitHub **auto-creates** a referenced environment on first use with no protection rules and no
branch policy, so relying on that instead of provisioning the list below produces exactly the gap
this section exists to prevent (see #141). The list below is generated from the actual
`environment:` expressions each workflow binds, not from what the environment happens to be named
-- keep the two in sync when a workflow's binding changes.

1. Go to **Settings** → **Environments**
2. Create environments matching every `environment:` binding in `.github/workflows/*.yml`:
   - `dev`, `staging`, `prod` -- bound by `deploy-aws-lambda.yml`, `deploy-gcp.yml`,
     `deploy-azure.yml`, and three of `rollback.yml`'s four jobs (`rollback-aws-lambda`,
     `rollback-gcp`, `rollback-azure` -- reusing the deploy environments rather than having their
     own, see #139). `deploy-aws-fargate.yml` and `rollback.yml`'s `rollback-aws-fargate` job both
     use `aws-fargate-<env>` instead (below), matching each other rather than this group.
   - `aws-fargate-dev`, `aws-fargate-staging`, `aws-fargate-prod` -- `deploy-aws-fargate.yml`
   - `aws-db-dev`, `aws-db-staging`, `aws-db-prod` -- `database-migration.yml` (AWS)
   - `gcp-db-dev`, `gcp-db-staging`, `gcp-db-prod` -- `database-migration.yml` (GCP)
   - `azure-db-dev`, `azure-db-staging`, `azure-db-prod` -- `database-migration.yml` (Azure)
   - `staging` -- also bound by `cleanup-staging.yml`'s destroy jobs (same `staging` environment as
     above, not a separate one)
   - `dev` -- also bound by `destroy-fargate-dev.yml` (same `dev` environment as above)
   - `frontend-aws-dev`, etc. -- if/when frontend deploy workflows gain an `environment:` binding

3. Configure protection rules:
   - **Production** (`prod`, `aws-fargate-prod`, `aws-db-prod`, `gcp-db-prod`, `azure-db-prod`):
     require approvals, restrict `deployment_branch_policy` to `main`
   - **Staging** (`staging`, `aws-fargate-staging`, `aws-db-staging`, `gcp-db-staging`,
     `azure-db-staging`): optional approvals, restrict to `main`
   - **Dev** (`dev`, `aws-fargate-dev`, `aws-db-dev`, `gcp-db-dev`, `azure-db-dev`): no restrictions

   Configuring protection rules here is necessary but not sufficient on its own: each cloud's OIDC
   trust must independently allowlist the `environment:<name>` subject for every name above, and
   the three clouds do this three different ways:
   - **Azure** (`terraform/environments/azure/ci-cd-permissions/`): the `github_environments`
     Terraform variable -- add the name there and re-apply, or `azure/login` fails with
     `AADSTS70021`.
   - **AWS** (`terraform/environments/aws/ci-cd-permissions/role.tf`): a literal `sub` list inlined
     in the role's assume-role policy, not a variable -- add the subject there and re-apply, or
     `configure-aws-credentials` fails with `AssumeRoleWithWebIdentity` denied.
   - **GCP** (`terraform/environments/gcp/ci-cd-permissions/github_oidc.tf`): ref-based, not
     environment-based -- its `attribute_condition` checks `repository`/`ref` only, never `sub`, so
     no GCP-side change is needed for a new environment name (see #141 for why that's its own,
     separate gap).

---

## Troubleshooting

### CI Workflow Fails

**Unit tests fail:**

```bash
# Run locally
make test-unit
```

**Integration tests fail:**

```bash
# Run with testcontainers
make test-integration
```

**Security scan fails:**

```bash
# Run locally
make security-scan-all
```

### Deployment Fails

**AWS - Image not found:**

```bash
# Check ECR
aws ecr describe-images --repository-name cudly --region us-east-1

# Re-push image
docker push <account>.dkr.ecr.us-east-1.amazonaws.com/cudly:latest
```

**GCP - Permission denied:**

```bash
# Check service account permissions
gcloud projects get-iam-policy <project-id>
```

The deploy service account's roles come from `local.deploy_roles` in
`terraform/environments/gcp/ci-cd-permissions/service_account.tf`. If a role is missing, add it
there and re-apply that bootstrap module rather than granting it by hand.

**Azure - Resource not found:**

```bash
# Verify resource group exists
az group show --name cudly-rg

# Create if missing
az group create --name cudly-rg --location eastus
```

### Database Migration Fails

**Connection timeout:**

- Check database security groups/firewall rules
- Verify VPN/bastion access if required
- Check database is running

**Migration already applied:**

```bash
# Check current version
migrate -path migrations -database <url> version

# Force version (use with caution)
migrate -path migrations -database <url> force <version>
```

---

## Best Practices

### 1. Branch Protection

- Require CI to pass before merging
- Require code reviews
- Restrict direct pushes to main

### 2. Environment Strategy

- **Dev**: Auto-deploy on push to develop branch
- **Staging**: Auto-deploy on push to main
- **Prod**: Manual approval required, deploy on release

### 3. Rollback Strategy

- Keep last 10 images in each registry
- Document rollback procedures
- Test rollback in staging first

### 4. Monitoring

- Set up CloudWatch/Cloud Logging alerts
- Monitor deployment success rates
- Track deployment frequency

### 5. Security

- Rotate secrets regularly
- Use environment protection rules
- Enable secret scanning
- Review security scan results

---

## Metrics & Monitoring

### Workflow Success Rate

```bash
# View recent workflow runs
gh run list --limit 50

# View specific workflow
gh run list --workflow=ci.yml --limit 20
```

### Deployment Frequency

- Target: Multiple deployments per day
- Track via GitHub Actions insights

### Mean Time to Recovery (MTTR)

- Use rollback workflow for quick recovery
- Target: < 15 minutes

### CI Duration

- Unit tests: ~5 min
- Integration tests: ~3 min
- Security scans: ~2 min
- Total: ~10 min target

---

## Additional Resources

- [GitHub Actions Documentation](https://docs.github.com/en/actions)
- [AWS ECR Documentation](https://docs.aws.amazon.com/ecr/)
- [GCP Artifact Registry](https://cloud.google.com/artifact-registry/docs)
- [Azure Container Registry](https://docs.microsoft.com/en-us/azure/container-registry/)
- [golang-migrate](https://github.com/golang-migrate/migrate)
- [Terraform Cloud](https://www.terraform.io/cloud)
