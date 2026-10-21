package server

import (
	"context"
	"testing"
	"time"

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
