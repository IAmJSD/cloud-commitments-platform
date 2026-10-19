package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeCollectionPartial(t *testing.T) {
	assert.Nil(t, decodeCollectionPartial(nil), "NULL means the last collection was complete")

	got := decodeCollectionPartial([]byte(`{"providers":[{"provider":"azure","incomplete_accounts":1,"attempted_scopes":3,"failed_scopes":2,"failed_details":0,"reasons":{"throttled":2}}]}`))
	require.NotNil(t, got)
	assert.Equal(t, []PartialProviderSweep{{
		Provider: "azure", IncompleteAccounts: 1, AttemptedScopes: 3, FailedScopes: 2,
		Reasons: map[string]int{PartialReasonThrottled: 2},
	}}, got.Providers)

	assert.Nil(t, decodeCollectionPartial([]byte(`not json`)),
		"an undecodable diagnostic must not fail the freshness read")
}
