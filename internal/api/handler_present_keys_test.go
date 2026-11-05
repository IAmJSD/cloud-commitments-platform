package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encoding/json binds "Term" to the term field, so presentKeys must see it as
// present too, otherwise the overlay silently drops a value that was decoded.
func TestPresentKeys_CaseInsensitive(t *testing.T) {
	got, err := presentKeys(`{"Term":3,"PAYMENT":null,"coverage":80}`, []string{"term", "payment", "coverage", "enabled"})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"term": true, "payment": true, "coverage": true}, got)
}
