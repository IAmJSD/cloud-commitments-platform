package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDerivePlanProviders(t *testing.T) {
	plan := &PurchasePlan{
		Services: map[string]ServiceConfig{
			"aws/ec2":       {},
			"aws/rds":       {},
			"azure/compute": {},
		},
	}

	got, err := DerivePlanProviders(plan)
	require.NoError(t, err)
	assert.Equal(t, []string{"aws", "azure"}, got)

	got, err = DerivePlanProviders(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = DerivePlanProviders(&PurchasePlan{})
	require.NoError(t, err)
	assert.Empty(t, got, "a plan with no services restricts nothing")
}

// TestDerivePlanProviders_ColonKeys covers the "provider:service" keys that
// migration 000040 wrote (issue #316, A04-012). They name a provider just as
// clearly as the "provider/service" keys buildServiceConfig writes, and
// findServiceKey in internal/api already treats them as valid. Skipping them
// left a colon-keyed plan with an empty provider set, which both
// account-assignment guards read as "no restriction".
func TestDerivePlanProviders_ColonKeys(t *testing.T) {
	plan := &PurchasePlan{
		Services: map[string]ServiceConfig{
			"aws:savings-plans-compute":     {},
			"aws:savings-plans-ec2instance": {},
			"gcp/compute":                   {},
		},
	}

	got, err := DerivePlanProviders(plan)
	require.NoError(t, err)
	assert.Equal(t, []string{"aws", "gcp"}, got)
}

// TestDerivePlanProviders_UnparseableKeys: a key that names no provider is
// skipped when other keys do name one (skipping can only narrow the set, and
// plans may carry entries this version does not know). When no key names a
// provider the result must be an error, not an empty set: both callers read
// an empty set as "accept an account of any provider" (issue #316, A04-012).
func TestDerivePlanProviders_UnparseableKeys(t *testing.T) {
	for _, key := range []string{"malformed", "/ec2", ":ec2", ""} {
		t.Run("only "+key, func(t *testing.T) {
			got, err := DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{key: {}}})
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "names a provider")
		})
		t.Run("alongside aws/ec2 "+key, func(t *testing.T) {
			got, err := DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{
				"aws/ec2": {},
				key:       {},
			}})
			require.NoError(t, err)
			assert.Equal(t, []string{"aws"}, got)
		})
	}
}
