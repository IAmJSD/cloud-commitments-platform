package api

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsCfgLoadStub stands in for awsconfig.LoadDefaultConfig: like the SDK's
// IMDS lookup it fails with the context's error once the context is done, and
// it records the context it ran under.
type awsCfgLoadStub struct {
	ctx   context.Context
	err   error // ctx.Err() at the time of the load
	calls int
}

func (s *awsCfgLoadStub) load(ctx context.Context) (aws.Config, error) {
	s.calls++
	s.ctx, s.err = ctx, ctx.Err()
	if s.err != nil {
		return aws.Config{}, s.err
	}
	return aws.Config{Region: "eu-west-1"}, nil
}

// Issue #104 (audit finding A02-017): awsCfgOnce loaded the base AWS config
// with the first caller's request context and memoized the error, so one
// request canceled or timed out mid-load sealed "context canceled" into the
// handler for the life of the process. The load now runs detached from the
// request's cancellation, bounded by awsBaseConfigLoadTimeout.
func TestBaseAWSConfig_CanceledFirstRequestDoesNotPoisonTheCache(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	entryPoints := []struct {
		call func(h *Handler, ctx context.Context) error
		name string
	}{
		{name: "getBaseAWSConfig", call: func(h *Handler, ctx context.Context) error {
			_, err := h.getBaseAWSConfig(ctx)
			return err
		}},
		{name: "loadAWSConfigWithRegion", call: func(h *Handler, ctx context.Context) error {
			_, err := h.loadAWSConfigWithRegion(ctx, "us-east-1")
			return err
		}},
		{name: "getLambdaInvoker", call: func(h *Handler, ctx context.Context) error {
			_, err := h.getLambdaInvoker(ctx)
			return err
		}},
	}
	for _, entry := range entryPoints {
		t.Run(entry.name, func(t *testing.T) {
			stub := &awsCfgLoadStub{}
			h := &Handler{loadAWSCfg: stub.load}

			require.NoError(t, entry.call(h, canceled), "a canceled request must not fail the shared config load")
			require.Equal(t, 1, stub.calls)
			require.NoError(t, stub.err, "the load must not inherit the request's cancellation")
			deadline, ok := stub.ctx.Deadline()
			require.True(t, ok, "the detached load must still be bounded")
			assert.WithinDuration(t, time.Now().Add(awsBaseConfigLoadTimeout), deadline, 5*time.Second)

			cfg, err := h.getBaseAWSConfig(context.Background())
			require.NoError(t, err, "the cached config must not hold the canceled request's error")
			assert.Equal(t, "eu-west-1", cfg.Region)
			assert.Equal(t, 1, stub.calls, "the config is still loaded once")
		})
	}
}
