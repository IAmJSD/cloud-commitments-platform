package server

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCleanupStarter struct{ ctx context.Context }

func (f *fakeCleanupStarter) StartCleanupWorker(ctx context.Context) { f.ctx = ctx }

func TestStartRateLimiterCleanup_SurvivesRequestCtxAndStopsOnClose(t *testing.T) {
	app := &Application{}
	first := &fakeCleanupStarter{}

	app.startRateLimiterCleanup(first)

	require.NotNil(t, first.ctx)
	assert.NoError(t, first.ctx.Err(), "worker ctx must not depend on any request ctx")

	second := &fakeCleanupStarter{}
	app.startRateLimiterCleanup(second)
	assert.Error(t, first.ctx.Err(), "a retried start replaces the previous worker")
	assert.NoError(t, second.ctx.Err())

	require.NoError(t, app.Close())
	select {
	case <-second.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("worker ctx not canceled on Close")
	}
}

// dbRLWorkerCreator is the "created by" frame every goroutine launched by
// api.DBRateLimiter.StartCleanupWorker carries in a full stack dump.
const dbRLWorkerCreator = "created by github.com/LeanerCloud/cloud-commitments-platform/internal/api.(*DBRateLimiter).StartCleanupWorker"

// liveDBRLWorkers counts the DBRateLimiter cleanup goroutines running now.
func liveDBRLWorkers() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), dbRLWorkerCreator)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// newReinitTestApp builds a pre-DB Application whose reinitializeAfterConnect
// runs without a database, AWS or secret store: the stores and services it
// wires only touch the pool when used, the encryption key comes from the raw
// env var, and the AWS config loads from empty files.
func newReinitTestApp(t *testing.T) *Application {
	t.Helper()
	for _, key := range []string{
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ARN", "CREDENTIAL_ENCRYPTION_KEY_SECRET_NAME",
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ID", "CREDENTIAL_ENCRYPTION_ALLOW_DEV_KEY",
		"CUDLY_SIGNING_KEY_ID", "CUDLY_SIGNING_KEY_VAULT_URL", "CUDLY_SIGNING_KEY_NAME",
		"CUDLY_SIGNING_KEY_RESOURCE", "AWS_PROFILE", "AWS_LAMBDA_RUNTIME_API",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", strings.Repeat("12", 32))
	t.Setenv("SCHEDULED_TASK_AUTH_MODE", "disabled")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")

	app, err := NewApplicationFromDeps(testutil.TestContext(t), ApplicationConfig{
		DefaultTerm: 3, DefaultCoverage: 80, DashboardURL: "https://rl-cleanup.example.test",
	}, ExternalDeps{
		EmailSender: &noopEmailSender{},
		DBConfig:    &database.Config{Host: "localhost", Port: 5432, Database: "cudly_test", User: "test", Password: "test", SSLMode: "disable"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Close() }) // idempotent; covers early test failures
	return app
}

// TestReinitializeAfterConnect_CleanupWorkerOutlivesRequestCtx pins the call
// site (#1137): the worker reinitializeAfterConnect starts must not stop when
// the request context that triggered the lazy DB connect ends (#269), and must
// stop on Close. Starting it with dbRL.StartCleanupWorker(ctx) fails this.
func TestReinitializeAfterConnect_CleanupWorkerOutlivesRequestCtx(t *testing.T) {
	app := newReinitTestApp(t)
	before := liveDBRLWorkers()

	reqCtx, cancelReq := context.WithCancel(testutil.TestContext(t))
	require.NoError(t, app.reinitializeAfterConnect(reqCtx, &database.Connection{}))
	require.Equal(t, before+1, liveDBRLWorkers(), "reinitializeAfterConnect must start one cleanup worker")

	cancelReq()
	assert.Never(t, func() bool { return liveDBRLWorkers() <= before },
		300*time.Millisecond, 10*time.Millisecond,
		"cleanup worker stopped with the request ctx; it must run on the app lifetime ctx")

	require.NoError(t, app.Close())
	assert.Eventually(t, func() bool { return liveDBRLWorkers() == before },
		2*time.Second, 10*time.Millisecond, "cleanup worker must stop on Close")
}

// TestReinitializeAfterConnect_NoCleanupWorkerWhenLaterStepFails: ensureDB
// closes the pool when reinitializeAfterConnect fails, so a worker started
// before the failing step would tick against a closed pool until a retry
// replaced it or Close canceled it (#1137).
func TestReinitializeAfterConnect_NoCleanupWorkerWhenLaterStepFails(t *testing.T) {
	app := newReinitTestApp(t)
	// A missing shared-config profile fails the AWS config load, a step that
	// runs after the rate limiter is built.
	t.Setenv("AWS_PROFILE", "cudly-rl-cleanup-missing-profile")
	before := liveDBRLWorkers()

	err := app.reinitializeAfterConnect(testutil.TestContext(t), &database.Connection{})
	require.ErrorContains(t, err, "failed to load AWS config")
	assert.Equal(t, before, liveDBRLWorkers(), "a failed reinitializeAfterConnect must not leave a cleanup worker running")
}
