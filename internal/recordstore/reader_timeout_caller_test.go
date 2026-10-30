package recordstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTimeoutReaderCallerDeadlineIsNotUnreachable checks that a deadline the
// caller set, shorter than the per-read deadline, is not reported as an
// unreachable store.
func TestTimeoutReaderCallerDeadlineIsNotUnreachable(t *testing.T) {
	r := WithTimeout(fakeGetRunReader{block: true}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := r.GetRun(ctx, "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetRun error = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("a caller deadline must not become ErrUnavailable: %v", err)
	}
}
