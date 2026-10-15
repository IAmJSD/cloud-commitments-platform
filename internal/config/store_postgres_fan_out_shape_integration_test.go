//go:build integration
// +build integration

package config

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPinExecutionFanOutShape pins the issue #97 store contract against a real
// PostgreSQL: the first shape pinned for a lineage is returned to every later
// caller, including concurrent first callers, and invalid input is rejected.
func TestPinExecutionFanOutShape(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	t.Run("first writer wins", func(t *testing.T) {
		key := uuid.NewString()
		unpinned, err := store.GetExecutionFanOutShape(ctx, key)
		require.NoError(t, err)
		assert.Empty(t, unpinned, "a lineage nothing pinned reads as empty")

		got, err := store.PinExecutionFanOutShape(ctx, key, FanOutShapeSingle)
		require.NoError(t, err)
		assert.Equal(t, FanOutShapeSingle, got)

		got, err = store.PinExecutionFanOutShape(ctx, key, FanOutShapeMulti)
		require.NoError(t, err)
		assert.Equal(t, FanOutShapeSingle, got, "a later attempt must get the first attempt's shape back")

		read, err := store.GetExecutionFanOutShape(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, FanOutShapeSingle, read, "the read must return the pinned shape")
	})

	t.Run("concurrent first attempts agree", func(t *testing.T) {
		key := uuid.NewString()
		shapes := []string{FanOutShapeSingle, FanOutShapeMulti, FanOutShapeSingle, FanOutShapeMulti}
		got := make([]string, len(shapes))
		var wg sync.WaitGroup
		for i := range shapes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s, err := store.PinExecutionFanOutShape(ctx, key, shapes[i])
				assert.NoError(t, err)
				got[i] = s
			}(i)
		}
		wg.Wait()
		for i := range got {
			assert.Equal(t, got[0], got[i], "every concurrent caller must see the one pinned shape")
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		_, err := store.PinExecutionFanOutShape(ctx, "", FanOutShapeSingle)
		assert.Error(t, err)
		_, err = store.PinExecutionFanOutShape(ctx, uuid.NewString(), "both")
		assert.Error(t, err)
	})
}
