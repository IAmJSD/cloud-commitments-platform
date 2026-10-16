package scheduler

import "context"

// backgroundCollectionKey marks a ListRecommendations context whose caller
// may kick the cold-start or stale-cache background collection.
type backgroundCollectionKey struct{}

// WithBackgroundCollection returns a context that lets ListRecommendations
// kick its cold-start collection (taking the cluster-wide in-flight marker)
// or its stale-cache background refresh.
//
// Both kicks collect for every enabled provider and every registered account,
// so only a caller entitled to every account may trigger them: a reader
// restricted to a subset of accounts must not cause cloud calls against
// accounts outside its scope, nor hold the marker that makes other users'
// POST /api/recommendations/refresh return 409 (issue #627, the read-path
// counterpart of #520). Without this marker ListRecommendations is a pure
// cache read, so a caller that forgets to set it fails closed.
func WithBackgroundCollection(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundCollectionKey{}, true)
}

// BackgroundCollectionAllowed reports whether ctx was marked by
// WithBackgroundCollection. Exported so callers' tests can assert which reads
// were allowed to kick a collection.
func BackgroundCollectionAllowed(ctx context.Context) bool {
	allowed, ok := ctx.Value(backgroundCollectionKey{}).(bool)
	return ok && allowed
}
