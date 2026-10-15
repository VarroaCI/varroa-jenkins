package bus

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

const (
	listenerTestWildcard = "varroa.test.listener.>"
	listenerTestSubject  = "varroa.test.listener.one"
)

// listenerProbe counts the messages a Listener receives and keeps the last
// subject it saw.
type listenerProbe struct {
	got  atomic.Int64
	mu   sync.Mutex
	last string
}

func (p *listenerProbe) handle(subject string, _ []byte) {
	p.mu.Lock()
	p.last = subject
	p.mu.Unlock()
	p.got.Add(1)
}

func (p *listenerProbe) lastSubject() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// received publishes one message on the test subject and reports whether
// the probe receives it within 200 ms.
func received(t *testing.T, pub *Conn, p *listenerProbe) bool {
	t.Helper()
	before := p.got.Load()
	if err := pub.Publish(listenerTestSubject, []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if p.got.Load() > before {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestListen_RecoversAfterDeniedSubscribe pins the recovery from a plain
// subscription that NATS denies at the first subscribe. Listen returns no
// error, and the Listener receives once the reload allows the subject.
func TestListen_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := startResponderServer(t, listenerTestWildcard)
	lc := d.restricted()
	pub := d.client()

	var p listenerProbe
	l, err := lc.listen(listenerTestWildcard, "", p.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("a denied first subscribe must not return an error: %v", err)
	}
	defer func() { _ = l.Unsubscribe() }()
	if received(t, pub, &p) {
		t.Fatal("a denied listener received a message")
	}

	d.setDeny()
	bustest.Eventually(t, "the listener did not recover after the reload", func() bool {
		return received(t, pub, &p)
	})
	if got := watcherCount(lc, listenerTestWildcard); got != 1 {
		t.Fatalf("watchers after recovery = %d, want 1", got)
	}
}

// TestListen_RecoversAfterGrantRemovedAndRestored pins the recovery from a
// reload that removes the grant of a live queue subscription and a later
// reload that restores it.
func TestListen_RecoversAfterGrantRemovedAndRestored(t *testing.T) {
	d := startResponderServer(t)
	lc := d.restricted()
	pub := d.client()

	var p listenerProbe
	l, err := lc.listen(listenerTestWildcard, "workers", p.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Unsubscribe() }()
	bustest.Eventually(t, "the listener never received", func() bool {
		return received(t, pub, &p)
	})

	d.setDeny(listenerTestWildcard)
	bustest.Eventually(t, "the listener still receives after the grant was removed", func() bool {
		return !received(t, pub, &p)
	})

	d.setDeny()
	bustest.Eventually(t, "the listener did not recover after the grant was restored", func() bool {
		return received(t, pub, &p)
	})
}

// TestListen_QueueWildcardPassesConcreteSubject pins that a wildcard queue
// subscription hands the handler the subject of each message.
func TestListen_QueueWildcardPassesConcreteSubject(t *testing.T) {
	d := startResponderServer(t)
	lc := d.restricted()
	pub := d.client()

	var p listenerProbe
	l, err := lc.listen(listenerTestWildcard, "workers", p.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Unsubscribe() }()
	bustest.Eventually(t, "the listener never received", func() bool {
		return received(t, pub, &p)
	})
	if got := p.lastSubject(); got != listenerTestSubject {
		t.Fatalf("handler subject = %q, want %q", got, listenerTestSubject)
	}
}

// TestListen_IgnoresDenialOfOtherSubject pins that a denial of another
// subject does not rebuild an accepted Listener.
func TestListen_IgnoresDenialOfOtherSubject(t *testing.T) {
	d := startResponderServer(t, responderTestOther)
	lc := d.restricted()
	pub := d.client()

	var p listenerProbe
	l, err := lc.listen(listenerTestWildcard, "", p.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Unsubscribe() }()
	bustest.Eventually(t, "the listener never received", func() bool {
		return received(t, pub, &p)
	})
	first := l.sub

	other, err := lc.SubscribeData(responderTestOther, func([]byte) {})
	if err != nil {
		t.Fatalf("subscribe other: %v", err)
	}
	defer func() { _ = other.Unsubscribe() }()
	time.Sleep(10 * responderTestBackoff) // A wrong rebuild has time to happen.

	if !received(t, pub, &p) {
		t.Fatal("the listener stopped receiving after a denial of another subject")
	}
	if l.sub != first {
		t.Fatal("a denial of another subject rebuilt the listener")
	}
}

// TestListen_UnsubscribeReleasesWatcher pins that Unsubscribe stops the
// goroutine, releases the subscription and the watcher, and leaves no
// subscribe attempt behind it.
func TestListen_UnsubscribeReleasesWatcher(t *testing.T) {
	d := startResponderServer(t, listenerTestWildcard)
	lc := d.restricted()
	pub := d.client()

	var p listenerProbe
	l, err := lc.listen(listenerTestWildcard, "", p.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := l.Unsubscribe(); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	select {
	case <-l.done:
	default:
		t.Fatal("Unsubscribe returned before the goroutine exited")
	}
	if got := watcherCount(lc, listenerTestWildcard); got != 0 {
		t.Fatalf("watchers after Unsubscribe = %d, want 0", got)
	}

	d.setDeny()
	time.Sleep(10 * responderTestBackoff) // A stray rebuild has time to happen.
	if received(t, pub, &p) {
		t.Fatal("a subscribe attempt followed Unsubscribe")
	}
}
