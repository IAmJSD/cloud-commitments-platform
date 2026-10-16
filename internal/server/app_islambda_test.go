package server

import (
	"os"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lambdaEnvVars are the variables the AWS Lambda runtime sets. The issue #110
// tests set or clear all of them so the ambient environment always contradicts
// the ApplicationConfig under test.
var lambdaEnvVars = []string{
	"AWS_LAMBDA_RUNTIME_API",
	"AWS_LAMBDA_FUNCTION_NAME",
	"AWS_EXECUTION_ENV",
	"LAMBDA_TASK_ROOT",
}

// setLambdaEnv makes the process environment look like Lambda (onLambda) or
// like a container/local run (!onLambda). t.Setenv registers the restore
// before os.Unsetenv clears the variable, so the original value comes back
// after the test either way.
func setLambdaEnv(t *testing.T, onLambda bool) {
	t.Helper()
	values := map[string]string{
		"AWS_LAMBDA_RUNTIME_API":   "127.0.0.1:9001",
		"AWS_LAMBDA_FUNCTION_NAME": "cudly-islambda-test",
		"AWS_EXECUTION_ENV":        "AWS_Lambda_provided.al2023",
		"LAMBDA_TASK_ROOT":         "/var/task",
	}
	for _, key := range lambdaEnvVars {
		if onLambda {
			t.Setenv(key, values[key])
			continue
		}
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
}

// assertLambdaModeWiring asserts every Lambda-mode consumer the Application
// owns follows want, whatever the environment says.
func assertLambdaModeWiring(t *testing.T, app *Application, want bool) {
	t.Helper()
	assert.Equal(t, want, app.appConfig.IsLambda, "Application config")

	sched, ok := app.Scheduler.(*scheduler.Scheduler)
	require.True(t, ok, "app.Scheduler must be a *scheduler.Scheduler, got %T", app.Scheduler)
	assert.Equal(t, want, sched.IsLambda(), "scheduler must follow ApplicationConfig.IsLambda, not the live environment")

	require.NotNil(t, app.API, "API handler")
	assert.Equal(t, want, app.API.IsLambda(), "API handler must follow ApplicationConfig.IsLambda, not the live environment")
}

// TestNewApplicationFromDeps_IsLambdaSingleSourceOfTruth is the regression
// test for issue #110: NewApplicationFromDeps built the pre-DB scheduler with
// a live runtime.IsLambda() (and the API handler read it on every call), so a
// caller-supplied ApplicationConfig.IsLambda that disagreed with the
// environment put the scheduler on the wrong path until ensureDB succeeded,
// or permanently if the database stayed unreachable.
func TestNewApplicationFromDeps_IsLambdaSingleSourceOfTruth(t *testing.T) {
	tests := []struct {
		name        string
		cfgIsLambda bool
		envOnLambda bool
	}{
		{"config Lambda, environment not Lambda", true, false},
		{"config not Lambda, environment Lambda", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCHEDULED_TASK_AUTH_MODE", "disabled")
			setLambdaEnv(t, tt.envOnLambda)

			cfg := ApplicationConfig{
				Version:         "islambda-test",
				DefaultTerm:     3,
				DefaultCoverage: 80,
				// A configured issuer URL skips the Lambda function-URL
				// lookup, so the test makes no AWS call in either mode.
				DashboardURL: "https://islambda.example.test",
				IsLambda:     tt.cfgIsLambda,
			}
			deps := ExternalDeps{
				EmailSender: &noopEmailSender{},
				DBConfig:    &database.Config{Host: "localhost", Port: 5432, Database: "cudly_test", User: "test", Password: "test", SSLMode: "disable"},
			}

			app, err := NewApplicationFromDeps(testutil.TestContext(t), cfg, deps)
			require.NoError(t, err)
			require.Nil(t, app.DB, "the pre-DB wiring is what this test covers")

			assertLambdaModeWiring(t, app, tt.cfgIsLambda)
		})
	}
}
