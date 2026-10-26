package credentials

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyResolver fails its first failFirst GetSecret calls, and any call made
// with an already-canceled context, then serves value. It is safe for
// concurrent use.
type flakyResolver struct {
	fakeResolver
	calls     atomic.Int32
	failFirst int32
}

var errThrottled = errors.New("ThrottlingException: rate exceeded")

func (f *flakyResolver) GetSecret(ctx context.Context, _ string) (string, error) {
	n := f.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if n <= f.failFirst {
		return "", errThrottled
	}
	return f.value, nil
}

// A failed key fetch must not be memoized: one throttled or canceled fetch at
// cold start would otherwise break credential decryption for the rest of the
// process lifetime (issue #104). A successful load is still memoized.
func TestLoadKey_FailedLoadIsRetried(t *testing.T) {
	resetKeyCacheForTest()
	t.Cleanup(resetKeyCacheForTest)
	clearAllKeyEnvs(t)
	t.Setenv(EnvSecretARN, "arn:aws:secretsmanager:us-east-1:123:secret:enc-key-AbC")
	r := &flakyResolver{fakeResolver: fakeResolver{value: validHexKey}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := LoadKey(canceled, r)
	require.ErrorIs(t, err, context.Canceled, "the first caller's own canceled context fails its own load")

	key, source, err := LoadKey(context.Background(), r)
	require.NoError(t, err, "a later caller must retry instead of getting the cached failure")
	assert.Len(t, key, 32)
	assert.Equal(t, EnvSecretARN, source)

	again, _, err := LoadKey(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, key, again)
	assert.Equal(t, int32(2), r.calls.Load(), "a successful load is memoized: no third fetch")
}

// Concurrent callers racing a failing first fetch: loads are serialized, so
// exactly one caller sees the throttle, exactly one more fetch succeeds, and
// every other caller is served the memoized key without fetching again.
func TestLoadKey_ConcurrentCallersAfterTransientFailure(t *testing.T) {
	resetKeyCacheForTest()
	t.Cleanup(resetKeyCacheForTest)
	clearAllKeyEnvs(t)
	t.Setenv(EnvSecretARN, "arn:aws:secretsmanager:us-east-1:123:secret:enc-key-AbC")
	r := &flakyResolver{fakeResolver: fakeResolver{value: validHexKey}, failFirst: 1}

	const callers = 32
	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		failures  atomic.Int32
		successes atomic.Int32
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, _, err := LoadKey(context.Background(), r)
			if err != nil {
				assert.ErrorIs(t, err, errThrottled)
				failures.Add(1)
				return
			}
			assert.Len(t, key, 32)
			successes.Add(1)
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), failures.Load(), "only the caller whose fetch was throttled fails")
	assert.Equal(t, int32(callers-1), successes.Load())
	assert.Equal(t, int32(2), r.calls.Load(), "one failed fetch, one successful fetch, then the memoized key")
}
