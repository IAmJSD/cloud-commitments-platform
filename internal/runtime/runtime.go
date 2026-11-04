// Package runtime holds small helpers that inspect the process's
// runtime environment. Kept deliberately minimal — this is a shared
// surface, not a grab-bag for utility code.
package runtime //nolint:revive // var-naming: package name "runtime" is intentional; provides process-env helpers distinct from stdlib runtime

import "os"

// IsLambda reports whether the current process is running inside an
// AWS Lambda execution environment. Detection is based on
// AWS_LAMBDA_RUNTIME_API, which the Lambda service sets for every
// function, zip or container image alike (the runtime interface client
// in a Lambda container image needs it). It is absent on non-Lambda
// deployments: local dev runs and the long-running servers (Fargate,
// Cloud Run, Container Apps).
//
// The flag answers one question only: is the process frozen between
// invocations, so that background goroutines (e.g. the
// stale-while-revalidate refresh) cannot be relied on to finish? It
// says nothing about whether the deployment runs a single instance or
// many, so do not use it to decide whether process-local state is
// shared across requests.
//
// Callers asking the freeze question should use this helper rather
// than reading the env var directly so the detection rule stays
// consistent across call sites.
func IsLambda() bool {
	return os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
}
