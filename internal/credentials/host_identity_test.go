package credentials

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingCallerIdentitySTS struct{}

func (blockingCallerIdentitySTS) GetCallerIdentity(ctx context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestVerifyHostAccount_ErrorDoesNotLeakHostIdentity(t *testing.T) {
	const hostID = "111111111111"
	const rawSTSError = "operation error STS: GetCallerIdentity, https response error StatusCode: 403"
	account := &config.CloudAccount{ID: "tenant-acct", ExternalID: "222222222222", AWSAuthMode: "role_arn"}

	for name, tc := range map[string]struct {
		hostSTS CallerIdentityClient
		want    error
	}{
		"different host account": {&callerIdentitySTS{account: hostID}, ErrNotHostAccount},
		"sts error":              {&callerIdentitySTS{err: errors.New(rawSTSError + " arn:aws:iam::" + hostID + ":role/cudly")}, ErrHostIdentityUnavailable},
		"empty host account":     {&callerIdentitySTS{}, ErrHostIdentityUnavailable},
		"no sts client":          {nil, ErrHostIdentityUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			err := VerifyHostAccount(context.Background(), account, tc.hostSTS)
			require.ErrorIs(t, err, ErrNotHostAccount, "every failure must still fail closed as ErrNotHostAccount")
			assert.Equal(t, tc.want.Error(), err.Error())
			assert.NotContains(t, err.Error(), hostID)
			assert.NotContains(t, err.Error(), "StatusCode")
			assert.NotContains(t, err.Error(), "GetCallerIdentity")
		})
	}
}

func TestVerifyHostAccount_BoundsSTSCall(t *testing.T) {
	old := hostIdentityTimeout
	hostIdentityTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hostIdentityTimeout = old })

	account := &config.CloudAccount{ID: "tenant-acct", ExternalID: "222222222222", AWSAuthMode: "role_arn"}
	done := make(chan error, 1)
	go func() { done <- VerifyHostAccount(context.Background(), account, blockingCallerIdentitySTS{}) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNotHostAccount)
		assert.Equal(t, ErrHostIdentityUnavailable.Error(), err.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyHostAccount did not return after the STS timeout")
	}
}

// recordingBlockingSTS blocks until its ctx is done, then reports the ctx
// error it saw, so a test can tell a caller cancellation from the
// hostIdentityTimeout deadline.
type recordingBlockingSTS struct {
	started chan struct{}
	seen    chan error
}

func (s recordingBlockingSTS) GetCallerIdentity(ctx context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	close(s.started)
	<-ctx.Done()
	s.seen <- ctx.Err()
	return nil, ctx.Err()
}

// Issue #559: the STS call must inherit the caller's ctx, so a request the
// client cancels stops the call at once instead of running for up to
// hostIdentityTimeout. The timeout is raised far above the test's wait, so
// only the caller's cancellation can end the call in time.
func TestVerifyHostAccount_PropagatesCallerCancellation(t *testing.T) {
	old := hostIdentityTimeout
	hostIdentityTimeout = time.Hour
	t.Cleanup(func() { hostIdentityTimeout = old })

	account := &config.CloudAccount{ID: "tenant-acct", ExternalID: "222222222222", AWSAuthMode: "role_arn"}
	fake := recordingBlockingSTS{started: make(chan struct{}), seen: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- VerifyHostAccount(ctx, account, fake) }()

	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyHostAccount never called STS")
	}
	cancel()

	select {
	case seen := <-fake.seen:
		require.ErrorIs(t, seen, context.Canceled, "the STS call must see the caller's cancellation, not a deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("the STS call did not see the caller's cancellation")
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNotHostAccount)
		assert.Equal(t, ErrHostIdentityUnavailable.Error(), err.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyHostAccount did not return after the caller canceled")
	}
}

// Issue #559: only a genuine account mismatch tells the tenant to set
// aws_role_arn. A check that could not run is the platform's problem, so its
// text must not ask for a config change, and the two must stay distinct.
func TestVerifyHostAccount_OnlyMismatchSuggestsRoleARN(t *testing.T) {
	const hostID = "111111111111"
	account := &config.CloudAccount{ID: "tenant-acct", ExternalID: "222222222222", AWSAuthMode: "role_arn"}

	mismatch := VerifyHostAccount(context.Background(), account, &callerIdentitySTS{account: hostID})
	require.ErrorIs(t, mismatch, ErrNotHostAccount)
	assert.NotErrorIs(t, mismatch, ErrHostIdentityUnavailable)
	assert.Contains(t, mismatch.Error(), "set aws_role_arn")

	for name, hostSTS := range map[string]CallerIdentityClient{
		"sts error":          &callerIdentitySTS{err: errors.New("sts down")},
		"sts timeout":        &callerIdentitySTS{err: context.DeadlineExceeded},
		"empty host account": &callerIdentitySTS{},
		"no sts client":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			err := VerifyHostAccount(context.Background(), account, hostSTS)
			require.ErrorIs(t, err, ErrHostIdentityUnavailable)
			require.ErrorIs(t, err, ErrNotHostAccount, "an unverified host must still fail closed")
			assert.NotContains(t, err.Error(), "aws_role_arn")
			assert.NotEqual(t, ErrNotHostAccount.Error(), err.Error())
		})
	}
}
