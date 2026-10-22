package config

// BuildRecommendationChunkQuery exposes the keyset chunk query to the external
// integration tests, which EXPLAIN it to prove recommendations_scan_key_idx
// (migration 000142) can serve it.
var BuildRecommendationChunkQuery = buildRecommendationChunkQuery
