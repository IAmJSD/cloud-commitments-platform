package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterRecommendationsByEnabledProviders(t *testing.T) {
	recs := []RecommendationRecord{{ID: "a", Provider: "aws"}, {ID: "z", Provider: "azure"}}
	assert.Equal(t, recs, FilterRecommendationsByEnabledProviders(recs, nil), "empty list is permissive")
	got := FilterRecommendationsByEnabledProviders(recs, []string{"azure"})
	assert.Equal(t, []RecommendationRecord{recs[1]}, got)
	assert.Empty(t, FilterRecommendationsByEnabledProviders(recs, []string{"gcp"}))
}
