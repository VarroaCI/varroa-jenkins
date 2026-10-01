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

// fakeDesiredInbox is a hand-driven desiredInbox. Tests push requests onto
// reqs and close dead to simulate a subscription that dies mid-connection.
type fakeDesiredInbox struct {
	reqs chan desiredRequest
	dead chan struct{}

	mu           sync.Mutex
	unsubscribed bool
}

func newFakeDesiredInbox() *fakeDesiredInbox {
	return &fakeDesiredInbox{reqs: make(chan desiredRequest, 8), dead: make(chan struct{})}
}

func (f *fakeDesiredInbox) Next(timeout time.Duration) (desiredRequest, error) {
	select {
	case r := <-f.reqs:
		return r, nil
	case <-f.dead:
		return desiredRequest{}, nats.ErrBadSubscription
	case <-time.After(timeout):
		return desiredRequest{}, nats.ErrTimeout
	}
}

func (f *fakeDesiredInbox) Unsubscribe() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribed = true
	return nil
}

// flakyDesiredSource fails the first failures calls to SubscribeDesired,
// then hands back inbox. It records every attempt.
type flakyDesiredSource struct {
	mu       sync.Mutex
	failures int
	attempts int
	inbox    *fakeDesiredInbox
}

func (f *flakyDesiredSource) SubscribeDesired(_ string) (desiredInbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failures {
		return nil, errors.New("context deadline exceeded")
	}
	return f.inbox, nil
}

func (f *flakyDesiredSource) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// swappableDesiredSource returns whatever its next func hands back.
type swappableDesiredSource struct {
	next func() (desiredInbox, error)
}

func (s *swappableDesiredSource) SubscribeDesired(_ string) (desiredInbox, error) {
	return s.next()
}

// desiredReq builds a request carrying a desired-state command with the
// given id, and a confirm func that records the answer on confirmed.
func desiredReq(t *testing.T, id string, confirmed chan<- string) desiredRequest {
	t.Helper()
	payload, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_DesiredState{
			DesiredState: &mitev1.DesiredStateCommand{CommandId: id},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return desiredRequest{Data: payload, Confirm: func() error {
		if confirmed != nil {
			confirmed <- id
		}
		return nil
	}}
}

// TestServeDesiredStateRetriesFailedSubscribe asserts that a transient
// subscribe failure at connect time does not starve the mite of desired
// state: the goroutine retries, and a later request still reaches the stream.
func TestServeDesiredStateRetriesFailedSubscribe(t *testing.T) {
	inbox := newFakeDesiredInbox()
	src := &flakyDesiredSource{failures: 2, inbox: inbox}

	h := &BusHandler{
		cluster:      "core",
		desired:      src,
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
	go h.serveDesiredState(ctx, "varroa", "varroa-ci", send, nil)

	waitFor(t, func() bool { return src.attemptCount() >= 3 }, 2*time.Second,
		"subscribe was never retried after a transient failure")

	inbox.reqs <- desiredReq(t, "cmd-1", nil)

	select {
	case op := <-delivered:
		if got := op.GetDesiredState().CommandId; got != "cmd-1" {
			t.Fatalf("command_id = %q, want cmd-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desired state never reached the mite stream after subscribe retry")
	}
}

// TestServeDesiredStateRepliesOnlyAfterSend asserts the delivery rule: the
// operator gets an answer only after the send to the mite stream succeeds.
// A failed send gets no answer, because another gateway replica may hold the
// live stream during a reconnect.
func TestServeDesiredStateRepliesOnlyAfterSend(t *testing.T) {
	inbox := newFakeDesiredInbox()
	h := &BusHandler{
		cluster:      "core",
		desired:      &flakyDesiredSource{inbox: inbox},
		watchBackoff: 5 * time.Millisecond,
		cancels:      make(map[string]context.CancelFunc),
	}

	var mu sync.Mutex
	failNext := true
	sent := make(chan string, 4)
	send := func(op *mitev1.OperatorMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if failNext {
			failNext = false
			return errors.New("stream closed")
		}
		sent <- op.GetDesiredState().CommandId
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.serveDesiredState(ctx, "varroa", "reply-ctl", send, inbox)

	confirmed := make(chan string, 4)
	inbox.reqs <- desiredReq(t, "lost", confirmed)
	inbox.reqs <- desiredReq(t, "kept", confirmed)

	select {
	case id := <-confirmed:
		if id != "kept" {
			t.Fatalf("confirmed %q; a failed send must get no answer", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a delivered command was never confirmed")
	}
	if id := <-sent; id != "kept" {
		t.Fatalf("sent %q, want kept", id)
	}
	select {
	case id := <-confirmed:
		t.Fatalf("unexpected second confirm %q", id)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestServeDesiredStateRebuildsLostSubscription covers the second half of
// the silent-starvation hole: a subscription that dies later must be rebuilt
// while the mite is still connected.
func TestServeDesiredStateRebuildsLostSubscription(t *testing.T) {
	first := newFakeDesiredInbox()
	second := newFakeDesiredInbox()

	var mu sync.Mutex
	calls := 0
	src := &swappableDesiredSource{next: func() (desiredInbox, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}}

	h := &BusHandler{
		cluster:       "core",
		desired:       src,
		watchBackoff:  5 * time.Millisecond,
		watchDegraded: make(map[string]map[string]string),
		miteVersion:   make(map[string]string),
		cancels:       make(map[string]context.CancelFunc),
	}

	delivered := make(chan *mitev1.OperatorMessage, 4)
	send := func(op *mitev1.OperatorMessage) error {
		delivered <- op
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.serveDesiredState(ctx, "varroa", "varroa-ci", send, nil)

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 1 }, 2*time.Second,
		"first subscription was never established")
	close(first.dead)

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 }, 2*time.Second,
		"subscription was not rebuilt after it died")
	first.mu.Lock()
	unsubscribed := first.unsubscribed
	first.mu.Unlock()
	if !unsubscribed {
		t.Fatal("the dead subscription was not released")
	}

	second.reqs <- desiredReq(t, "cmd-2", nil)

	select {
	case op := <-delivered:
		if got := op.GetDesiredState().CommandId; got != "cmd-2" {
			t.Fatalf("command_id = %q, want cmd-2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desired state never reached the mite stream after the rebuild")
	}
}

// TestServeDesiredStateMarksAndClearsDegraded asserts the failure is
// visible: a failing subscribe marks the controller degraded, and a
// successful retry clears it.
func TestServeDesiredStateMarksAndClearsDegraded(t *testing.T) {
	src := &flakyDesiredSource{failures: 3, inbox: newFakeDesiredInbox()}

	h := &BusHandler{
		cluster:      "core",
		desired:      src,
		watchBackoff: 20 * time.Millisecond,
		cancels:      make(map[string]context.CancelFunc),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.serveDesiredState(ctx, "varroa", "flaky-ctl", func(*mitev1.OperatorMessage) error { return nil }, nil)

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded("varroa/flaky-ctl")
		return degraded && reason != ""
	}, 2*time.Second, "subscribe failure was never marked degraded")

	waitFor(t, func() bool {
		_, degraded := h.streamDegraded("varroa/flaky-ctl")
		return !degraded
	}, 3*time.Second, "degraded flag was never cleared after the subscription recovered")
}

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
// mark would hide a still-broken desired-state watch, leaving it invisible.
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

// TestServeDesiredStateDoesNotSpinOnInstantDeath pins the rebuild pacer. A
// subscription whose setup succeeds but which dies immediately would
// otherwise loop at full speed: retryEstablish only backs off while setup is
// failing, and it succeeds here every time.
func TestServeDesiredStateDoesNotSpinOnInstantDeath(t *testing.T) {
	var mu sync.Mutex
	establishes := 0

	// Every subscription handed out is already dead.
	src := &swappableDesiredSource{next: func() (desiredInbox, error) {
		mu.Lock()
		establishes++
		mu.Unlock()
		in := newFakeDesiredInbox()
		close(in.dead)
		return in, nil
	}}

	h := &BusHandler{
		cluster:       "core",
		desired:       src,
		watchBackoff:  50 * time.Millisecond,
		watchDegraded: make(map[string]map[string]string),
		miteVersion:   make(map[string]string),
		cancels:       make(map[string]context.CancelFunc),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go h.serveDesiredState(ctx, "varroa", "spin-ctl", func(*mitev1.OperatorMessage) error { return nil }, nil)

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
		t.Fatalf("subscription was not rebuilt at all: %d attempts", got)
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
// leaving a Connected mite receiving no desired state.
func TestOnDisconnectIgnoresSupersededStream(t *testing.T) {
	h := &BusHandler{
		cluster:              "core",
		cancels:              make(map[string]context.CancelFunc),
		certExpiry:           make(map[string]time.Time),
		connectEpoch:         make(map[string]int64),
		idleGauges:           make(map[string]string),
		idleGaugesAt:         make(map[string]time.Time),
		installedPluginsHash: make(map[string]string),
		pendingAcks:          make(map[string]map[string]pendingImperative),
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
// subscription being released, which only happens when its goroutine exits.
func TestOnConnectCancelsSupersededConnection(t *testing.T) {
	first := newFakeDesiredInbox()
	second := newFakeDesiredInbox()

	var mu sync.Mutex
	calls := 0
	h := newTestBusHandler()
	h.desired = &swappableDesiredSource{next: func() (desiredInbox, error) {
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
	mu.Lock()
	opened := calls
	mu.Unlock()
	if opened != 1 {
		t.Fatalf("OnConnect opened %d desired subscriptions before it returned, want 1", opened)
	}

	// The mite reconnects before the old stream tore down.
	_ = h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)

	waitFor(t, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.unsubscribed
	}, 2*time.Second, "superseded connection's desired goroutine kept running after reconnect")
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
		pendingAcks:          make(map[string]map[string]pendingImperative),
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
