# Changelog

All notable changes to the CUDly platform (the API server, the web frontend
and the deployment IaC) are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/).

Releases up to 0.9.0 were cut from the single CUDly repository, before the
CLI, the shared Go libraries and providers, and the MCP server moved to
[cloud-commitments-cli](https://github.com/LeanerCloud/cloud-commitments-cli),
[cloud-commitments-go](https://github.com/LeanerCloud/cloud-commitments-go) and
[cloud-commitments-mcp](https://github.com/LeanerCloud/cloud-commitments-mcp).
Entries for code that now lives in those repositories have been removed here.

The Unreleased section is not a complete list of changes since 0.9.0. No
release has been cut since then, so the
[merged pull requests](https://github.com/LeanerCloud/cloud-commitments-platform/pulls?q=is%3Apr+is%3Amerged)
are the full record until the next release.

## [Unreleased]

### Notices

- **Federation IaC bundles downloaded before 2026-04-22 need to be
  re-downloaded** to get zero-touch registration. Older bundles silently
  skip auto-registration unless manually edited (Terraform `registration.tf`
  gated `do_register` on `cudly_api_url`; CLI shell scripts included the
  registration call only when `CUDlyAPIURL` was present at render time;
  CloudFormation deploy scripts had no registration call at all).
  Re-download the bundle from the CUDly UI and the new copy will register
  your account automatically with no manual edits required.
- **Federation IaC bundles deployed before #1219 need to be re-applied** to
  pick up reconciled IAM action grants. Older bundles silently degrade on
  federated accounts: the Cost Explorer `Get*Coverage` actions are missing
  (coverage-targeted sizing assumes zero existing coverage), and the
  cross-account CloudFormation flavor lacks the optional `EnableOrgDiscovery`
  parameter and the legacy CE statement that the original `CUDly-CrossAccount`
  template carried. Re-download the federation bundle from the CUDly UI and
  re-apply (`terraform apply` / `aws cloudformation update-stack`) -- no
  manual edits required, no changes to existing CUDly resources. Customers
  on the runtime CloudFormation stack or Terraform lambda/fargate modules
  also need an update to pick up the new `ec2:*ReservedInstancesExchangeQuote`
  actions, `ec2:DescribeRegions`, `rds:DescribeDBInstances`, and the
  new-style `es:*ReservedInstance*` OpenSearch actions (replacing the legacy
  `es:*ReservedElasticsearch*` names).

### Fixed

- Account scopes match `allowed_accounts` entries by account ID only. A
  display-name entry used to grant whichever account was later created or
  renamed to that name. Group writes now store the ID of the one account a
  name identifies (and reject names that identify none or several), and
  migration 000145 rewrites existing name entries whose account predates the
  grant; entries it cannot bind no longer match any account by name. The
  purchase, dashboard, ladder and RI reshape scope checks decide on the
  record's cloud account ID rather than on a name looked up for it, and a name
  only identifies an account through that account's own, unambiguous external
  ID (#1034)
- API-key creation over HTTP rejects an explicit `max_amount: 0` spend cap with a 400 instead of minting the key with no cap, matching group writes (#980)
- Group writes reject an explicit `max_amount: 0` spend cap with a 400 instead of storing it as no cap; omit the field for no limit (#133)
- Remove debug console.log from frontend recommendation handler
- Align pre-commit gocyclo threshold (10) with CI pipeline
- Pin tool versions in GitHub Actions for reproducible builds
- The local git-secrets setup script aborted on its PEM pattern and, when
  patched past that, made every scan fail on an invalid regex; its keyword
  allowlist whitelisted whole Terraform and test-file lines. Allowlisting now
  lives in `.gitallowed` as literal entries and the PEM detector covers
  PKCS#8 keys (#1972)

## [0.9.0] - 2026-03-06

### Added

- RI Exchange feature: reshape analysis with normalization factors, API
  endpoints, and frontend page for managing convertible Reserved Instances
- RI utilization tracking from Cost Explorer with pagination support
- Security headers on all Lambda responses
- Admin password resolution from cloud secret managers

### Fixed

- Harden RI exchange handlers with validation and error sanitization
- Fix async race conditions and input validation in RI exchange frontend
- Fix base64 encoding for saveProfile and resetPassword
- Remove duplicate logout event handler
- Guard DNS zone outputs against missing resources (GCP, Azure)
- Fix Azure CDN redirect type and SPA routing
- Add network policies and resource quotas to AKS module
- Add security headers to Azure Front Door and GCP load balancer
- Wire admin password secrets through all cloud environment root modules

## [0.8.0] - 2026-02-01

### Added

- Deployment health check blocks for AWS, Azure, and GCP Terraform modules
- GCP self-signed cert for dev HTTPS
- Azure Front Door API routing and custom domain support
- Cross-provider deployment test harness script
- Azure ACR resource and registry authentication

### Fixed

- Enforce SSL-only connections on GCP Cloud SQL
- Migrate GCP load balancer to EXTERNAL_MANAGED with SPA routing
- Fix Azure Container Apps config and CDN delivery rule names
- Fix GCP frontend build trigger and database password generation
- Expand frontend CSP connect-src for Azure and GCP API origins
- Fix Fargate EventBridge container name
- Capture migration exit code correctly in entrypoint.sh

### Changed

- Convert AWS database from Aurora Serverless v2 to standalone RDS
- Move GCP Secret Manager out of database module
- Replace Azure Container App Jobs with Logic Apps scheduled tasks
- Simplify Azure database module

## [0.7.0] - 2026-01-15

### Added

- Full Terraform infrastructure for AWS (Fargate, Lambda, CloudFront, RDS),
  Azure (Container Apps, AKS, Front Door, PostgreSQL), and GCP (Cloud Run,
  GKE, Cloud SQL) with CI-specific tfvars
- PostgreSQL database with connection pool, migrations, and secret resolvers
- Authentication service with RBAC and API key support
- REST API with rate limiting, CORS, and middleware stack
- Email service with SMTP sender and cloud credential resolution
- Analytics collector, purchase execution, and scheduled task runner
- Docker containerization with multi-stage builds and compose configs
- GitHub Actions CI/CD pipeline (lint, test, security scan, Docker build,
  Terraform validate, E2E tests, Infracost)
- Frontend web dashboard with TypeScript, webpack, Chart.js

### Fixed

- Sanitize user input in dashboard and recommendations (XSS prevention)
- Add connection pool limits and graceful shutdown to server
- Enforce 12-char minimum password with complexity requirements
- Use hidden-source-map for production frontend builds
- Use rightmost X-Forwarded-For IP for client identification
- Add SHA256 checksum verification for migrate binary in Docker
- Tighten git-secrets patterns to reduce false positives

## [0.6.0] - 2025-11-01

### Added

- OSL-3.0 license and contributing guidelines

The other changes in this release were to the CLI and the provider code,
which now live in cloud-commitments-cli and cloud-commitments-go.

## [0.5.0] - 2025-09-01

Changes in this release were to the CLI and the provider code, which now
live in cloud-commitments-cli and cloud-commitments-go.

## [0.4.0] - 2025-07-01

Changes in this release were to the CLI and the provider code, which now
live in cloud-commitments-cli and cloud-commitments-go.

## [0.3.0] - 2025-05-01

Changes in this release were to the CLI and the provider code, which now
live in cloud-commitments-cli and cloud-commitments-go.
