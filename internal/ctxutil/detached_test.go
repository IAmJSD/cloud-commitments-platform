package ctxutil

import (
	"context"
	"testing"
	"time"
)

type ctxKey struct{}

func TestDetachedWithTimeout(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, "req-1"), time.Millisecond)
	cancelParent()

	start := time.Now()
	ctx, cancel := DetachedWithTimeout(parent, time.Minute)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("parent cancellation must not reach the detached context, got %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("detached context must carry its own deadline")
	}
	if d := deadline.Sub(start); d < 59*time.Second || d > 61*time.Second {
		t.Fatalf("deadline must be ~1m from now (not the parent's), got %s", d)
	}
	if got := ctx.Value(ctxKey{}); got != "req-1" {
		t.Fatalf("detached context must keep parent values, got %v", got)
	}

	cancel()
	if ctx.Err() == nil {
		t.Fatal("the returned CancelFunc must cancel the detached context")
	}
}
