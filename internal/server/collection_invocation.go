package server

import (
	"context"
	"log"
)

// claimCollectionInvocation runs once a token-carrying collect run holds the
// advisory lock (see HandleScheduledTask), and swaps the marker token it was
// invoked with for a fresh invocation-scoped one (issue #84). The marker
// token MarkCollectionStarted handed the refresh handler is shared by every
// delivery of the same async event, and Lambda async delivery is
// at-least-once. After the swap a lock-skipped duplicate delivery's
// releaseSkippedCollectionMarker no longer matches the marker, so it cannot
// clear it while this run is still collecting.
//
// When another run holds a fresh marker this run owns nothing, so it collects
// tokenless and never clears. A store error keeps the marker token: that is
// the pre-#84 behavior, and failing a collection the lock already admitted
// over a bookkeeping write would be worse.
func (app *Application) claimCollectionInvocation(ctx context.Context, taskType ScheduledTaskType, params ScheduledTaskParams) ScheduledTaskParams {
	if taskType != TaskCollectRecommendations || params.OwnerToken == "" || app.Config == nil {
		return params
	}
	token, ok, err := app.Config.ClaimCollectionInvocation(ctx, params.OwnerToken)
	if err != nil {
		log.Printf("Failed to claim collection marker for this invocation, keeping the marker token: %v", err)
		return params
	}
	if !ok {
		log.Println("Collection marker is held by another run; collecting without an owner token")
		return ScheduledTaskParams{}
	}
	return ScheduledTaskParams{OwnerToken: token}
}
