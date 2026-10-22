package bustest

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// PresenceWait is the bound for a fetch that expects one message. A fetch
// with batch 1 returns as soon as the message arrives, so the long bound
// costs nothing when the test passes.
const PresenceWait = 10 * time.Second

// FetchOne pulls one message from sub within wait. It fails the test when
// no message arrives. It does not ack the message.
func FetchOne(t testing.TB, sub *nats.Subscription, wait time.Duration) *nats.Msg {
	t.Helper()
	msgs, err := sub.Fetch(1, nats.MaxWait(wait))
	if err != nil {
		t.Fatalf("fetch one message within %v: %v", wait, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("fetched %d messages within %v, want 1", len(msgs), wait)
	}
	return msgs[0]
}
