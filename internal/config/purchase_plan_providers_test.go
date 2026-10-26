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

// TestDerivePlanProviders_UnidentifiableEntries: an entry whose key names no
// provider and whose Provider field is empty is skipped when other entries do
// identify one (skipping can only narrow the set, and plans may carry entries
// this version does not know). When no entry identifies a provider the result
// must be an error, not an empty set: both callers read an empty set as
// "accept an account of any provider" (issue #316, A04-012).
func TestDerivePlanProviders_UnidentifiableEntries(t *testing.T) {
	for _, key := range []string{"malformed", "/ec2", ":ec2", ""} {
		t.Run("only "+key, func(t *testing.T) {
			got, err := DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{key: {Service: "ec2", Provider: "  "}}})
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "identifies a provider")
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

// TestDerivePlanProviders_LegacyKeyUsesProviderField: a legacy alias kept
// under its original key (the singleton-alias edit path, #1123) has no
// provider in its key, but its ServiceConfig names one. That is the field the
// legacy edit path and plan step resolution go by, so it counts. Main skipped
// such keys (provider set empty, no restriction); the first #316 commit
// refused them outright.
func TestDerivePlanProviders_LegacyKeyUsesProviderField(t *testing.T) {
	got, err := DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{
		"original-key": {Provider: "azure", Service: "vm"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"azure"}, got)

	got, err = DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{
		"aws/ec2":      {Provider: "aws", Service: "ec2"},
		"original-key": {Provider: "azure", Service: "vm"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"aws", "azure"}, got, "every entry's provider counts, since plan step resolution buys for each")

	got, err = DerivePlanProviders(&PurchasePlan{Services: map[string]ServiceConfig{
		"gcp/compute": {Provider: "azure", Service: "compute"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"gcp"}, got, "a key that names a provider still wins, as before")
}
