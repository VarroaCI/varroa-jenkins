package mite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// streamDegraded reports whether the bus→stream bridge for a controller is
// currently marked broken, and why. key is "ns/name". Test-only accessor.
func (h *BusHandler) streamDegraded(key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reason := degradedReasonLocked(h.watchDegraded[key])
	return reason, reason != ""
}

// fakeKeyWatcher is a hand-driven nats.KeyWatcher. Tests push entries onto
// updates and close it to simulate a watcher that dies mid-connection.
type fakeKeyWatcher struct {
	updates chan nats.KeyValueEntry
	mu      sync.Mutex
	stopped bool
}

func newFakeKeyWatcher() *fakeKeyWatcher {
	return &fakeKeyWatcher{updates: make(chan nats.KeyValueEntry, 8)}
}

func (w *fakeKeyWatcher) Context() context.Context           { return nil }
func (w *fakeKeyWatcher) Updates() <-chan nats.KeyValueEntry { return w.updates }
func (w *fakeKeyWatcher) Error() <-chan error                { return nil }

func (w *fakeKeyWatcher) isStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}
func (w *fakeKeyWatcher) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	return nil
}

// fakeEntry is a minimal nats.KeyValueEntry carrying a Put operation.
type fakeEntry struct {
	key   string
	value []byte
}

func (e *fakeEntry) Bucket() string             { return "mite_desired" }
func (e *fakeEntry) Key() string                { return e.key }
func (e *fakeEntry) Value() []byte              { return e.value }
func (e *fakeEntry) Revision() uint64           { return 1 }
func (e *fakeEntry) Created() time.Time         { return time.Time{} }
func (e *fakeEntry) Delta() uint64              { return 0 }
func (e *fakeEntry) Operation() nats.KeyValueOp { return nats.KeyValuePut }

// flakyDesiredKV fails the first failures calls to Watch, then hands back a
// live watcher. It records every attempt so the test can assert on retries.
type flakyDesiredKV struct {
	mu        sync.Mutex
	failures  int
	attempts  int
	watcher   *fakeKeyWatcher
	attempted chan struct{}
}

func (f *flakyDesiredKV) Watch(_ string) (nats.KeyWatcher, error) {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	select {
	case f.attempted <- struct{}{}:
	default:
	}
	if n <= f.failures {
		return nil, errors.New("context deadline exceeded")
	}
	return f.watcher, nil
}

func (f *flakyDesiredKV) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// TestWatchDesiredStateRetriesFailedWatchSetup pins the fix for #509: a
// transient Watch() setup failure at connect time must not permanently starve
// the mite of desired state. Before the fix the goroutine logged and returned,
// so the desired state published after the failure never reached the stream.
func TestWatchDesiredStateRetriesFailedWatchSetup(t *testing.T) {
	w := newFakeKeyWatcher()
	kv := &flakyDesiredKV{failures: 2, watcher: w, attempted: make(chan struct{}, 16)}

	h := &BusHandler{
		cluster:      "core",
		desiredKV:    kv,
		watchBackoff: 5 * time.Millisecond,
		cancels:      make(map[string]context.CancelFunc),
	}

	delivered := make(chan *mitev1.OperatorMessage, 4)
	send := func(op *mitev1.OperatorMessage) error {
		delivered <- op
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchDesiredState(ctx, "varroa", "varroa-ci", send)

	// Wait for the watch to be established (3rd attempt succeeds).
	waitFor(t, func() bool { return kv.attemptCount() >= 3 }, 2*time.Second,
		"watch setup was never retried after transient failure")

	// Desired state published after the transient failure must still arrive.
	payload, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{
			Imperative: &mitev1.ImperativeCommand{CommandId: "cmd-1"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w.updates <- &fakeEntry{key: "core/varroa/varroa-ci", value: payload}

	select {
	case op := <-delivered:
		if got := op.GetImperative().CommandId; got != "cmd-1" {
			t.Fatalf("command_id = %q, want cmd-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desired state never reached the mite stream after watch retry")
	}
}

// TestWatchDesiredStateReestablishesClosedWatch covers the second half of the
// silent-starvation hole: a watcher that dies later (server disconnect closes
// Updates()) must be re-established while the mite is still connected.
func TestWatchDesiredStateReestablishesClosedWatch(t *testing.T) {
	first := newFakeKeyWatcher()
	second := newFakeKeyWatcher()

	var mu sync.Mutex
	calls := 0
	kv := &swappableDesiredKV{next: func() (nats.KeyWatcher, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}}

	h := &BusHandler{
		cluster:      "core",
		desiredKV:    kv,
		watchBackoff: 5 * time.Millisecond,
		cancels:      make(map[string]context.CancelFunc),
	}

	delivered := make(chan *mitev1.OperatorMessage, 4)
	send := func(op *mitev1.OperatorMessage) error {
		delivered <- op
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchDesiredState(ctx, "varroa", "varroa-ci", send)

	// Kill the first watcher the way a server disconnect would.
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 1 }, 2*time.Second,
		"first watch was never established")
	close(first.updates)

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 }, 2*time.Second,
		"watch was not re-established after the updates channel closed")

	payload, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{
			Imperative: &mitev1.ImperativeCommand{CommandId: "cmd-2"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second.updates <- &fakeEntry{key: "core/varroa/varroa-ci", value: payload}

	select {
	case op := <-delivered:
		if got := op.GetImperative().CommandId; got != "cmd-2" {
			t.Fatalf("command_id = %q, want cmd-2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desired state never reached the mite stream after watch re-establish")
	}
}

// TestWatchDesiredStateMarksAndClearsDegraded asserts the failure is visible:
// a failing watch setup marks the controller degraded, and a successful
// re-establish clears it. Retry alone is not the fix — an exhausted or
// flapping watch must stop the CR from claiming health.
func TestWatchDesiredStateMarksAndClearsDegraded(t *testing.T) {
	w := newFakeKeyWatcher()
	kv := &flakyDesiredKV{failures: 3, watcher: w, attempted: make(chan struct{}, 16)}

	h := &BusHandler{
		cluster:      "core",
		desiredKV:    kv,
		watchBackoff: 20 * time.Millisecond,
		cancels:      make(map[string]context.CancelFunc),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchDesiredState(ctx, "varroa", "flaky-ctl", func(*mitev1.OperatorMessage) error { return nil })

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded("varroa/flaky-ctl")
		return degraded && reason != ""
	}, 2*time.Second, "watch failure was never marked degraded")

	waitFor(t, func() bool {
		_, degraded := h.streamDegraded("varroa/flaky-ctl")
		return !degraded
	}, 3*time.Second, "degraded flag was never cleared after the watch recovered")
}

// swappableDesiredKV returns whatever its next func hands back.
type swappableDesiredKV struct {
	next func() (nats.KeyWatcher, error)
}

func (s *swappableDesiredKV) Watch(_ string) (nats.KeyWatcher, error) { return s.next() }

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestRetryEstablishRetriesUntilSuccess pins the shared retry helper used by
// every bus->stream watcher. Setup failures must back off and retry rather
// than returning, and must mark the controller degraded while failing.
func TestRetryEstablishRetriesUntilSuccess(t *testing.T) {
	h := &BusHandler{cluster: "core", watchBackoff: 5 * time.Millisecond}

	var attempts int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ok := h.retryEstablish(ctx, "team-a", "imp-ctl", "imperative", func() error {
		attempts++
		if attempts < 3 {
			return errors.New("context deadline exceeded")
		}
		return nil
	})

	if !ok {
		t.Fatal("retryEstablish returned false despite eventual success")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if _, degraded := h.streamDegraded("team-a/imp-ctl"); degraded {
		t.Fatal("degraded flag should be cleared once setup succeeds")
	}
}

// TestRetryEstablishMarksDegradedWhileFailing asserts the failure is visible
// during the retry window, not only after it resolves.
func TestRetryEstablishMarksDegradedWhileFailing(t *testing.T) {
	h := &BusHandler{cluster: "core", watchBackoff: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.retryEstablish(ctx, "team-b", "content-ctl", "content", func() error {
		return errors.New("no responders")
	})

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded("team-b/content-ctl")
		return degraded && strings.Contains(reason, "no responders")
	}, 2*time.Second, "persistent setup failure was never marked degraded")
}

// TestRetryEstablishStopsOnContextCancel asserts the retry loop is bounded by
// the connection lifetime and does not leak a goroutine per disconnect.
func TestRetryEstablishStopsOnContextCancel(t *testing.T) {
	h := &BusHandler{cluster: "core", watchBackoff: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- h.retryEstablish(ctx, "team-c", "cancel-ctl", "desired", func() error {
			return errors.New("still failing")
		})
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("retryEstablish returned true after context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryEstablish did not stop when the context was cancelled")
	}
}

// TestWatchKindsTrackDegradationIndependently pins that the three bus watchers
// do not share one degraded flag. A healthy content subscription clearing the
// mark would hide a still-broken desired-state watch — exactly the invisibility
// #509 is about.
func TestWatchKindsTrackDegradationIndependently(t *testing.T) {
	h := &BusHandler{
		cluster:       "core",
		watchBackoff:  5 * time.Millisecond,
		watchDegraded: make(map[string]map[string]string),
		miteVersion:   make(map[string]string),
	}

	h.markWatchDegraded(context.Background(), "varroa", "multi-ctl", "desired", "desired watch setup failing: deadline exceeded")
	h.markWatchDegraded(context.Background(), "varroa", "multi-ctl", "content", "content watch setup failing: no responders")

	// The content watch recovers; the desired watch is still down.
	h.clearWatchDegraded(context.Background(), "varroa", "multi-ctl", "content")

	reason, degraded := h.streamDegraded("varroa/multi-ctl")
	if !degraded {
		t.Fatal("controller reported healthy while its desired-state watch is still broken")
	}
	if strings.Contains(reason, "content") {
		t.Fatalf("recovered content watch still reported as degraded: %q", reason)
	}
	if !strings.Contains(reason, "desired") {
		t.Fatalf("reason lost the still-broken desired watch: %q", reason)
	}

	// Once the last broken watch recovers the controller reports healthy.
	h.clearWatchDegraded(context.Background(), "varroa", "multi-ctl", "desired")
	if _, degraded := h.streamDegraded("varroa/multi-ctl"); degraded {
		t.Fatal("controller still degraded after every watch recovered")
	}
}

// TestWatchDesiredStateDoesNotSpinOnInstantDeath pins the rebuild pacer. A
// watch whose setup succeeds but which dies immediately would otherwise loop at
// full speed: retryEstablish only backs off while setup is *failing*, and it
// succeeds here every time.
func TestWatchDesiredStateDoesNotSpinOnInstantDeath(t *testing.T) {
	var mu sync.Mutex
	establishes := 0

	// Every watcher handed out is already dead, so each pump returns instantly.
	kv := &swappableDesiredKV{next: func() (nats.KeyWatcher, error) {
		mu.Lock()
		establishes++
		mu.Unlock()
		w := newFakeKeyWatcher()
		close(w.updates)
		return w, nil
	}}

	h := &BusHandler{
		cluster:       "core",
		desiredKV:     kv,
		watchBackoff:  50 * time.Millisecond,
		watchDegraded: make(map[string]map[string]string),
		miteVersion:   make(map[string]string),
		cancels:       make(map[string]context.CancelFunc),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go h.watchDesiredState(ctx, "varroa", "spin-ctl", func(*mitev1.OperatorMessage) error { return nil })

	time.Sleep(300 * time.Millisecond)
	cancel()

	mu.Lock()
	got := establishes
	mu.Unlock()

	// With 50ms base backoff doubling each rebuild, ~300ms allows a handful of
	// attempts. An unpaced loop reaches thousands.
	if got > 10 {
		t.Fatalf("rebuild loop spun: %d establish attempts in 300ms, want <= 10", got)
	}
	if got < 2 {
		t.Fatalf("watch was not rebuilt at all: %d attempts", got)
	}
}

// TestMarkWatchDegradedIgnoresCancelledConnection pins that a watch goroutine
// left over from a previous connection cannot stamp the *current* one degraded.
// A setup call blocks for seconds, so a fast disconnect/reconnect can land
// entirely inside one attempt; without the guard the new connection inherits a
// False alarm that nothing clears until its own watch dies.
func TestMarkWatchDegradedIgnoresCancelledConnection(t *testing.T) {
	h := &BusHandler{
		cluster:       "core",
		watchDegraded: make(map[string]map[string]string),
		miteVersion:   make(map[string]string),
		lastHeartbeat: make(map[string]time.Time),
	}

	stale, cancelStale := context.WithCancel(context.Background())
	cancelStale() // the old connection is gone

	h.markWatchDegraded(stale, "varroa", "reconnect-ctl", "desired", "old connection's failure")

	if reason, degraded := h.streamDegraded("varroa/reconnect-ctl"); degraded {
		t.Fatalf("a cancelled connection's goroutine marked the controller degraded: %q", reason)
	}

	// The live connection's own goroutine still works normally.
	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	h.markWatchDegraded(live, "varroa", "reconnect-ctl", "desired", "live failure")
	if _, degraded := h.streamDegraded("varroa/reconnect-ctl"); !degraded {
		t.Fatal("the live connection's goroutine failed to mark the controller degraded")
	}
}

// TestOnDisconnectIgnoresSupersededStream pins that a late teardown from a
// stream the mite has already replaced cannot tear down the live connection.
// Without the token check it cancels the new connection's watch goroutines,
// leaving a Connected mite receiving no desired state — #509's symptom reached
// by a different route.
func TestOnDisconnectIgnoresSupersededStream(t *testing.T) {
	h := &BusHandler{
		cluster:              "core",
		cancels:              make(map[string]context.CancelFunc),
		certExpiry:           make(map[string]time.Time),
		connectEpoch:         make(map[string]int64),
		idleGauges:           make(map[string]string),
		idleGaugesAt:         make(map[string]time.Time),
		installedPluginsHash: make(map[string]string),
		pendingAcks:          make(map[string]map[string]*nats.Msg),
		replayCmds:           make(map[string]map[string]bool),
		pendingContent:       make(map[string]*nats.Msg),
		pendingContentNS:     make(map[string]string),
		pendingContentTime:   make(map[string]time.Time),
		watchDegraded:        make(map[string]map[string]string),
		miteVersion:          make(map[string]string),
		lastHeartbeat:        make(map[string]time.Time),
		presenceLocks:        make(map[string]*sync.Mutex),
	}

	send := func(*mitev1.OperatorMessage) error { return nil }

	oldToken := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	// The mite reconnects on a fresh stream before the old one tears down.
	newToken := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	if oldToken == newToken {
		t.Fatal("reconnect reused the previous connection token")
	}

	// The superseded stream's teardown arrives late.
	h.OnDisconnect("ctl", "varroa", oldToken)

	h.mu.Lock()
	_, stillConnected := h.cancels["varroa/ctl"]
	h.mu.Unlock()
	if !stillConnected {
		t.Fatal("a superseded stream's teardown tore down the live connection")
	}

	// The live stream's own teardown still works.
	h.OnDisconnect("ctl", "varroa", newToken)
	h.mu.Lock()
	_, connected := h.cancels["varroa/ctl"]
	h.mu.Unlock()
	if connected {
		t.Fatal("the live stream's teardown did not disconnect the controller")
	}
}

// TestOnConnectCancelsSupersededConnection pins that a reconnect stops the
// previous connection's watch goroutines. Their cancel func is overwritten in
// h.cancels, and the old stream's OnDisconnect is (correctly) ignored as
// superseded — so without cancelling here they run for the life of the gateway
// pod, forwarding desired state into a dead stream. Proven by the first
// watcher being stopped, which only happens when its goroutine exits.
func TestOnConnectCancelsSupersededConnection(t *testing.T) {
	first := newFakeKeyWatcher()
	second := newFakeKeyWatcher()

	var mu sync.Mutex
	calls := 0
	h := newTestBusHandler()
	h.desiredKV = &swappableDesiredKV{next: func() (nats.KeyWatcher, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}}

	send := func(*mitev1.OperatorMessage) error { return nil }

	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 1 }, 2*time.Second,
		"first connection never established its desired watch")

	// The mite reconnects before the old stream tore down.
	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)

	waitFor(t, first.isStopped, 2*time.Second,
		"superseded connection's watch goroutine kept running after reconnect")
}

// newTestBusHandler builds a BusHandler with every map initialised and no bus
// dependencies, for tests that exercise connect/disconnect bookkeeping.
func newTestBusHandler() *BusHandler {
	return &BusHandler{
		cluster:              "core",
		cancels:              make(map[string]context.CancelFunc),
		certExpiry:           make(map[string]time.Time),
		connectEpoch:         make(map[string]int64),
		idleGauges:           make(map[string]string),
		idleGaugesAt:         make(map[string]time.Time),
		installedPluginsHash: make(map[string]string),
		pendingAcks:          make(map[string]map[string]*nats.Msg),
		replayCmds:           make(map[string]map[string]bool),
		pendingContent:       make(map[string]*nats.Msg),
		pendingContentNS:     make(map[string]string),
		pendingContentTime:   make(map[string]time.Time),
		watchDegraded:        make(map[string]map[string]string),
		miteVersion:          make(map[string]string),
		lastHeartbeat:        make(map[string]time.Time),
		presenceLocks:        make(map[string]*sync.Mutex),
	}
}

// TestOnConnectIssuesStrictlyIncreasingTokens pins that two connects can never
// share a token, even within one wall-clock tick: a duplicate would make a
// superseded stream's teardown look current and tear down the live connection.
func TestOnConnectIssuesStrictlyIncreasingTokens(t *testing.T) {
	h := newTestBusHandler()
	send := func(*mitev1.OperatorMessage) error { return nil }

	seen := make(map[int64]bool)
	prev := int64(0)
	for i := 0; i < 50; i++ {
		tok := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
		if seen[tok] {
			t.Fatalf("connect %d reissued token %d", i, tok)
		}
		if tok <= prev {
			t.Fatalf("connect %d issued non-increasing token %d after %d", i, tok, prev)
		}
		seen[tok] = true
		prev = tok
	}
}

// TestOnConnectDropsSupersededDegradedMarks pins that a new connection does not
// inherit the previous one's degraded marks. The new stream establishes its own
// watches; a leftover mark would ride into its first presence write and, if the
// matching watcher never runs setup again, never be cleared.
func TestOnConnectDropsSupersededDegradedMarks(t *testing.T) {
	h := newTestBusHandler()
	send := func(*mitev1.OperatorMessage) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	h.markWatchDegraded(ctx, "varroa", "ctl", "desired", "old connection's failure")
	if _, degraded := h.streamDegraded("varroa/ctl"); !degraded {
		t.Fatal("setup failed: mark was not recorded")
	}

	// The mite reconnects.
	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)

	if reason, degraded := h.streamDegraded("varroa/ctl"); degraded {
		t.Fatalf("new connection inherited the superseded connection's mark: %q", reason)
	}
}

// TestPresenceLocksRetiredOnDisconnect pins that the presence-lock map tracks
// live connections rather than every controller the gateway has ever served.
// It must not leak on disconnect, and must not be resurrected by a goroutine
// left over from a superseded connection.
func TestPresenceLocksRetiredOnDisconnect(t *testing.T) {
	h := newTestBusHandler()
	send := func(*mitev1.OperatorMessage) error { return nil }

	token := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	h.mu.Lock()
	_, installed := h.presenceLocks["varroa/ctl"]
	h.mu.Unlock()
	if !installed {
		t.Fatal("OnConnect did not install a presence lock")
	}

	h.OnDisconnect("ctl", "varroa", token)

	h.mu.Lock()
	_, leaked := h.presenceLocks["varroa/ctl"]
	h.mu.Unlock()
	if leaked {
		t.Fatal("presence lock leaked after disconnect")
	}

	// A stale write from the departed connection must not resurrect the entry.
	h.putPresence("varroa", "ctl", "v1")
	h.mu.Lock()
	_, resurrected := h.presenceLocks["varroa/ctl"]
	h.mu.Unlock()
	if resurrected {
		t.Fatal("a stale presence write resurrected the lock entry")
	}
}

// TestPresenceLockReusedAcrossReconnect pins that a reconnect keeps the same
// presence mutex. A fresh one would leave an in-flight write from the
// superseded connection holding the old mutex while the replacement writes
// under the new one, so the two writers no longer exclude each other and the
// stale write can land last.
func TestPresenceLockReusedAcrossReconnect(t *testing.T) {
	h := newTestBusHandler()
	send := func(*mitev1.OperatorMessage) error { return nil }

	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	first, ok := h.presenceLock("varroa/ctl")
	if !ok {
		t.Fatal("OnConnect did not install a presence lock")
	}

	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	second, ok := h.presenceLock("varroa/ctl")
	if !ok {
		t.Fatal("reconnect dropped the presence lock")
	}

	if first != second {
		t.Fatal("reconnect swapped in a new presence mutex; concurrent writers no longer exclude each other")
	}
}
