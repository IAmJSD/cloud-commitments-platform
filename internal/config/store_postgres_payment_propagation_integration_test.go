//go:build integration
// +build integration

package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A default payment valid globally but not for AWS ("monthly") must not be
// copied onto AWS rows (#539).
func TestIntegration_PropagateDefaultPayment_SkipsValueAWSRejects(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	global.DefaultPayment = "no-upfront"
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	require.NoError(t, store.SaveServiceConfig(ctx, &ServiceConfig{
		Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80, RampSchedule: RampImmediate,
	}))

	_, err = store.UpdateGlobalConfigAtomic(ctx, func(c *GlobalConfig) error {
		c.DefaultPayment = "monthly"
		return nil
	})
	require.NoError(t, err)

	svc, err := store.GetServiceConfig(ctx, "aws", "ec2")
	require.NoError(t, err)
	require.Equal(t, "no-upfront", svc.Payment)
}
