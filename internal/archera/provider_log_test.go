package archera

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type diagnosticResolver struct{ flakyResolver }

func (r *diagnosticResolver) GetSecret(ctx context.Context, ref string) (string, error) {
	key, err := r.flakyResolver.GetSecret(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("synthetic-backend-detail reading %s: %w", ref, err)
	}
	return key, nil
}

func TestClient_ResolverFailureLogsOnlySafeStage(t *testing.T) {
	var logs bytes.Buffer
	previousOutput, previousLevel := logging.SetOutput(&logs), logging.GetLevel()
	t.Cleanup(func() {
		logging.SetOutput(previousOutput)
		logging.SetLevelValue(previousLevel)
	})
	logging.SetLevelValue(logging.LevelWarn)
	rt := &hostAssertingTransport{t: t}
	resolver := &diagnosticResolver{flakyResolver{fails: 1}}
	p := NewProvider(fullSettings(), resolver, &http.Client{Transport: rt})

	client, err := p.Client(context.Background())
	require.ErrorIs(t, err, ErrKeyUnavailable)
	assert.Nil(t, client)
	for _, forbidden := range []string{testKey, "synthetic-backend-detail", testSecret, "vault down while reading"} {
		assert.NotContains(t, logs.String(), forbidden, "resolver diagnostic must not reach logs")
		assert.NotContains(t, err.Error(), forbidden)
	}
	assert.Contains(t, logs.String(), "[WARN] archera: credential resolution failed\n")
	assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("[WARN]")))
	assert.Zero(t, rt.calls.Load())

	client, err = p.Client(context.Background())
	require.NoError(t, err)
	require.NotNil(t, client)
	again, err := p.Client(context.Background())
	require.NoError(t, err)
	assert.Same(t, client, again)
	assert.EqualValues(t, 2, resolver.calls.Load())
	assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("[WARN]")))
	assert.Zero(t, rt.calls.Load())
}

func TestClient_ConstructorFailureLogsOnlySafeStage(t *testing.T) {
	for _, tc := range []struct {
		name, key, org, diagnostic string
	}{
		{"internal nonprintable key", testKey + "\x01suffix", testOrg, "archera API key contains a non-printable or non-ASCII character at byte 32"},
		{"invalid organization", testKey, "synthetic-invalid-org", "archera org ID must be a UUID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousOutput, previousLevel := logging.SetOutput(&logs), logging.GetLevel()
			t.Cleanup(func() {
				logging.SetOutput(previousOutput)
				logging.SetLevelValue(previousLevel)
			})
			logging.SetLevelValue(logging.LevelWarn)
			rt := &hostAssertingTransport{t: t}
			hc := &http.Client{Transport: rt}
			control, controlErr := insurance.NewClient(insurance.Config{APIKey: insurance.NewSecret(tc.key), OrgID: tc.org}, hc)
			require.EqualError(t, controlErr, tc.diagnostic)
			require.Nil(t, control)
			assert.NotContains(t, controlErr.Error(), tc.key)
			assert.Zero(t, rt.calls.Load())

			settings := fullSettings()
			settings.OrgID = tc.org
			resolver := envResolver(t)
			t.Setenv(testSecret, tc.key)
			p := NewProvider(settings, resolver, hc)
			client, err := p.Client(context.Background())
			require.ErrorIs(t, err, ErrKeyUnavailable)
			assert.Nil(t, client)
			for _, forbidden := range []string{tc.key, tc.org, testPlan, testSecret, tc.diagnostic} {
				assert.NotContains(t, logs.String(), forbidden)
				assert.NotContains(t, err.Error(), forbidden)
			}
			assert.Contains(t, logs.String(), "[WARN] archera: client initialization failed\n")
			assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("[WARN]")))
			assert.Zero(t, rt.calls.Load())

			if tc.org == testOrg {
				t.Setenv(testSecret, testKey)
				client, err = p.Client(context.Background())
				require.NoError(t, err)
				require.NotNil(t, client)
				again, err := p.Client(context.Background())
				require.NoError(t, err)
				assert.Same(t, client, again)
				assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("[WARN]")))
				assert.Zero(t, rt.calls.Load())
			}
		})
	}
}
