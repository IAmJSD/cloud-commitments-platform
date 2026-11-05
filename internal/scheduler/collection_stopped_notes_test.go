package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// The fixed collection-stopped notes are shown as written on the read path.
func TestCollectionStoppedNotes_ShowAsWritten(t *testing.T) {
	for _, n := range []string{collectionStoppedLoadingSettings, collectionStoppedIncomplete, collectionStoppedSaving} {
		assert.Equal(t, n, purchase.SanitizeStoredError(n))
		assert.Equal(t, n+"; "+n, purchase.SanitizeStoredError(n+"; "+n))
	}
}

// A failed rows write stores a fixed note; the database error text stays in
// the log.
func TestPersistCollection_StoresNoRawWriteError(t *testing.T) {
	raw := fmt.Errorf("write recommendations: failed to connect to `host=db-prod.internal user=cudly_app database=cudly`: " +
		"dial error (dial tcp 10.20.30.40:5432: connect: connection refused)")
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("UpsertRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(raw).Once()
	var stored string
	store.On("SetRecommendationsCollectionError", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { stored = args.String(1) }).Return(nil).Once()
	s := &Scheduler{config: store}

	s.persistCollection(context.Background(), nil, nil, nil, nil)

	assert.Equal(t, collectionStoppedSaving, stored)
	assert.Equal(t, stored, purchase.SanitizeStoredError(stored))
	for _, needle := range []string{"db-prod.internal", "cudly_app", "10.20.30.40", "dial"} {
		assert.NotContains(t, stored, needle)
	}
}
