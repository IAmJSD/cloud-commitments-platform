# CUDly self-hosted platform

The platform provides a self-hosted backend API, scheduled work, a web dashboard, and cloud deployment definitions. It uses the published [shared Go libraries](https://github.com/LeanerCloud/cloud-commitments-go) for provider operations, pinned to fixed versions in `go.mod`.

The dashboard and provider integrations are experimental. Building the components does not provision infrastructure, create a database, or start the application.

## Build the server

Use the Go version declared in `go.mod`. The shared Go modules are published and pinned in `go.mod`, so this checkout builds on its own.

```bash
make build-server
./bin/cudly-server --help
```

`make build-server` creates `bin/cudly-server` from `./cmd/server`. The target builds the server only. It does not deploy or start it.

To build the Lambda entry point for a deployment workflow:

```bash
make build-lambda
```

`make build-lambda` creates the `bootstrap` artifact from `./cmd/lambda`. Follow [deployment](docs/DEPLOYMENT.md) guidance before using it.

## Frontend

The dashboard lives under [`frontend`](frontend). Its package scripts and dependencies are defined in [`frontend/package.json`](frontend/package.json). Use the [development guide](docs/DEVELOPMENT.md) for local frontend work.

The dashboard is experimental. It shows recommendations, purchase plans, execution history, inventory, coverage, and account settings. Review provider and purchase state before enabling an operation.

Per-account service overrides are available from **Settings -> Accounts**. Expand an account, open **Service overrides**, select a provider and service, set term, payment, coverage, or enabled, and save. Blank fields inherit the global purchasing defaults. The same settings are available through `PUT /api/accounts/{id}/service-overrides/{provider}/{service}`.

## Owned directories

- `cmd/server` contains the HTTP server entry point.
- `cmd/lambda` contains the Lambda entry point.
- `internal` contains platform services and API code.
- `frontend` contains the web dashboard.
- `terraform` contains the deployment definitions; install CUDly with it (see [deployment](docs/DEPLOYMENT.md)). There is no CloudFormation install.
- `cloudformation`, `arm`, and `iac` contain the templates that create CUDly's role in each target cloud account. They do not install CUDly.

This component does not provide the `cudly` CLI. Use the [CLI component](https://github.com/LeanerCloud/cloud-commitments-cli) for command-line workflows.

## Provider and deployment caveats

- Amazon RDS and ElastiCache are the tested AWS service paths.
- Other AWS service paths, Azure, and GCP support remains experimental.
- Azure and GCP support is experimental and can vary by service and account.
- Deployment definitions can create billable cloud resources. Review the target, credentials, and plan before applying them.

Read [deployment](docs/DEPLOYMENT.md) and [development](docs/DEVELOPMENT.md) documentation before changing infrastructure or application behavior.

## Related components

- [CLI](https://github.com/LeanerCloud/cloud-commitments-cli) provides command-line workflows.
- [Shared Go libraries](https://github.com/LeanerCloud/cloud-commitments-go) provide provider and common packages.
- [MCP server](https://github.com/LeanerCloud/cloud-commitments-mcp) provides a separate tool interface.

## License and attribution

CUDly is maintained by [LeanerCloud](https://github.com/LeanerCloud) and licensed under the [Open Software License 3.0](LICENSE). See the repository license and attribution files for third-party notices.
