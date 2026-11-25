package mite

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// presenceTestHandler builds a BusHandler over an embedded NATS server and
// returns it with its presence bucket.
func presenceTestHandler(t *testing.T) (*BusHandler, *bus.KV) {
	t.Helper()
	natsSrv := startNATS(t)
	busConn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)
	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, bus.PresenceTTL)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}
	h := mustNewBusHandler(t, busConn, snapshotKV, presenceKV)
	t.Cleanup(h.Close)
	return h, presenceKV
}

// readPresenceRecord returns the presence record of the mite name in the
// namespace "ns" and its KV revision. ok is false when the key holds no
// record.
func readPresenceRecord(t *testing.T, kv *bus.KV, name string) (bus.Presence, uint64, bool) {
	t.Helper()
	entry, err := kv.KeyValue().Get(bus.PresenceKey(bus.DefaultCluster, "ns", name))
	if err != nil {
		return bus.Presence{}, 0, false
	}
	var p bus.Presence
	if err := json.Unmarshal(entry.Value(), &p); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	return p, entry.Revision(), true
}

func noSend(*mitev1.OperatorMessage) error { return nil }

func TestPresenceContract(t *testing.T) {
	if 3*bus.PresenceRefreshInterval > bus.PresenceTTL {
		t.Errorf("PresenceTTL %s must hold three refresh intervals of %s", bus.PresenceTTL, bus.PresenceRefreshInterval)
	}
	if bus.PresenceTTL >= bus.PresenceStaleAfter {
		t.Errorf("PresenceTTL %s must stay below PresenceStaleAfter %s, so a stopped gateway reads as missing",
			bus.PresenceTTL, bus.PresenceStaleAfter)
	}
}

func TestRefreshPresenceCarriesLastHeartbeat(t *testing.T) {
	h, kv := presenceTestHandler(t)
	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })

	old := time.Now().Add(-10 * time.Minute)
	h.mu.Lock()
	h.lastHeartbeat["ns/ctl"] = old
	h.presenceWrittenAt["ns/ctl"] = old
	h.mu.Unlock()

	seen, ok := h.refreshPresence("ctl", "ns", token, time.Minute, time.Hour, recvLoop)
	if !ok {
		t.Fatal("refreshPresence answered not current for the live token")
	}
	if !seen.Equal(old) {
		t.Errorf("seen = %s, want the stored heartbeat %s", seen, old)
	}
	p, _, found := readPresenceRecord(t, kv, "ctl")
	if !found {
		t.Fatal("no presence record after the refresh")
	}
	if !p.LastHeartbeat.Equal(old) {
		t.Errorf("record LastHeartbeat = %s, want %s, never the write time", p.LastHeartbeat, old)
	}
	if !p.StreamOpen || p.Epoch != token {
		t.Errorf("record = %+v, want StreamOpen and epoch %d", p, token)
	}
}

func TestRefreshPresenceSkipsAfterARecentWrite(t *testing.T) {
	h, kv := presenceTestHandler(t)
	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })

	_, before, found := readPresenceRecord(t, kv, "ctl")
	if !found {
		t.Fatal("OnConnect wrote no presence record")
	}
	if _, ok := h.refreshPresence("ctl", "ns", token, time.Hour, time.Hour, recvLoop); !ok {
		t.Fatal("refreshPresence answered not current for the live token")
	}
	if _, after, _ := readPresenceRecord(t, kv, "ctl"); after != before {
		t.Errorf("revision %d -> %d: the refresh wrote although the connect write is younger than one interval", before, after)
	}
}

func TestRefreshPresenceStopsForAnOldConnection(t *testing.T) {
	h, kv := presenceTestHandler(t)
	first := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	second := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	if _, ok := h.refreshPresence("ctl", "ns", first, 0, time.Hour, recvLoop); ok {
		t.Error("refreshPresence answered current for a superseded token")
	}

	h.OnDisconnect("ctl", "ns", second)
	if _, ok := h.refreshPresence("ctl", "ns", second, 0, time.Hour, recvLoop); ok {
		t.Error("refreshPresence answered current after the disconnect")
	}
	if _, _, found := readPresenceRecord(t, kv, "ctl"); found {
		t.Error("a refresh after the disconnect wrote the presence record again")
	}
}

// TestPresenceWriteSkipsASupersededEpoch pins that a timer write carrying an
// old epoch leaves the record of the replacement connection alone.
func TestPresenceWriteSkipsASupersededEpoch(t *testing.T) {
	h, kv := presenceTestHandler(t)
	first := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	second := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", second) })

	_, before, found := readPresenceRecord(t, kv, "ctl")
	if !found {
		t.Fatal("OnConnect wrote no presence record")
	}
	if got := h.writePresence("ns", "ctl", first, presenceGuarded); got != presenceNewer {
		t.Errorf("write of a superseded epoch = %v, want presenceNewer", got)
	}
	if _, after, _ := readPresenceRecord(t, kv, "ctl"); after != before {
		t.Fatalf("revision %d -> %d: a superseded epoch wrote the record", before, after)
	}

	if got := h.writePresence("ns", "ctl", second, presenceGuarded); got != presenceWritten {
		t.Errorf("write of the live epoch = %v, want presenceWritten", got)
	}
	p, after, _ := readPresenceRecord(t, kv, "ctl")
	if after == before {
		t.Fatal("the live epoch did not write the record")
	}
	if p.Epoch != second {
		t.Errorf("record = %+v, want epoch %d", p, second)
	}
}

// recvLoop reports a read loop that waits for the next mite message.
func recvLoop() (string, time.Time) { return readLoopRecv, time.Now() }

// TestRefreshPresenceNamesTheReadLoopOfASilentStream pins the read-loop
// note of the presence record. A heartbeat older than the silent limit
// puts the read-loop state in the record, also within one interval of the
// last write. The same state writes nothing more. The next heartbeat
// clears the note, and a fresh heartbeat never sets it.
func TestRefreshPresenceNamesTheReadLoopOfASilentStream(t *testing.T) {
	h, kv := presenceTestHandler(t)
	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })

	since := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	blocked := func() (string, time.Time) { return "log_chunk", since }
	if _, ok := h.refreshPresence("ctl", "ns", token, time.Hour, time.Minute, blocked); !ok {
		t.Fatal("refreshPresence answered not current for the live token")
	}
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.ReadLoop != "" {
		t.Fatalf("record ReadLoop = %q for a fresh heartbeat, want none", p.ReadLoop)
	}

	h.mu.Lock()
	h.lastHeartbeat["ns/ctl"] = time.Now().Add(-2 * time.Minute)
	h.mu.Unlock()
	if _, ok := h.refreshPresence("ctl", "ns", token, time.Hour, time.Minute, blocked); !ok {
		t.Fatal("refreshPresence answered not current for the live token")
	}
	p, rev, _ := readPresenceRecord(t, kv, "ctl")
	if p.ReadLoop != "log_chunk" || !p.ReadLoopSince.Equal(since) {
		t.Fatalf("record ReadLoop = %q since %s, want log_chunk since %s, within one interval of the last write",
			p.ReadLoop, p.ReadLoopSince, since)
	}
	if _, ok := h.refreshPresence("ctl", "ns", token, time.Hour, time.Minute, blocked); !ok {
		t.Fatal("refreshPresence answered not current for the live token")
	}
	if _, again, _ := readPresenceRecord(t, kv, "ctl"); again != rev {
		t.Errorf("revision %d -> %d: the same read-loop state wrote again within one interval", rev, again)
	}

	h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.ReadLoop != "" || !p.ReadLoopSince.IsZero() {
		t.Errorf("record ReadLoop = %q since %s after a heartbeat, want none", p.ReadLoop, p.ReadLoopSince)
	}
}

func TestKeepPresenceWarnsForASilentStream(t *testing.T) {
	h, kv := presenceTestHandler(t)
	h.presenceRefreshEvery = 10 * time.Millisecond
	h.presenceSilentAfter = 50 * time.Millisecond
	var out lockedBuffer
	h.Logger = slog.New(slog.NewTextHandler(&out, nil))

	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })
	h.mu.Lock()
	connectedAt := h.lastHeartbeat["ns/ctl"]
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.KeepPresence(ctx, "ctl", "ns", token, func() (string, time.Time) {
			return "log_chunk", time.Now().Add(-time.Minute)
		})
	}()

	waitFor(t, func() bool {
		return strings.Contains(out.String(), "mite stream open with no heartbeat")
	}, 10*time.Second, "no WARN line for a silent open stream")
	line := out.String()
	for _, want := range []string{"level=WARN", "controller=ns/ctl", "readLoop=log_chunk", "readLoopFor=1m0s"} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
	p, _, found := readPresenceRecord(t, kv, "ctl")
	if !found || !p.StreamOpen || !p.LastHeartbeat.Equal(connectedAt) {
		t.Errorf("record = %+v found=%v, want StreamOpen with the connect heartbeat %s", p, found, connectedAt)
	}

	h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
	waitFor(t, func() bool {
		return strings.Contains(out.String(), "mite stream heard again")
	}, 10*time.Second, "no INFO line after the heartbeat came back")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("KeepPresence did not return after its context ended")
	}
}

func TestKeepPresenceReturnsWhenSuperseded(t *testing.T) {
	h, _ := presenceTestHandler(t)
	h.presenceRefreshEvery = 10 * time.Millisecond
	first := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.KeepPresence(context.Background(), "ctl", "ns", first, func() (string, time.Time) {
			return readLoopRecv, time.Now()
		})
	}()
	second := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", second) })

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("KeepPresence of a superseded connection did not return")
	}
}

// keeperHandler wraps a BusHandler. It records when KeepPresence returns
// and when OnDisconnect starts. Its OnHeartbeat waits for release while
// block is set, so a test can hold the read loop inside a handler.
type keeperHandler struct {
	*BusHandler
	block   bool
	release chan struct{}
	inside  chan struct{}

	mu     sync.Mutex
	events []string
	keeper chan struct{}
}

func newKeeperHandler(h *BusHandler, block bool) *keeperHandler {
	return &keeperHandler{BusHandler: h, block: block, release: make(chan struct{}),
		inside: make(chan struct{}, 1), keeper: make(chan struct{})}
}

func (k *keeperHandler) record(event string) {
	k.mu.Lock()
	k.events = append(k.events, event)
	k.mu.Unlock()
}

func (k *keeperHandler) recorded() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.events...)
}

func (k *keeperHandler) KeepPresence(ctx context.Context, name, ns string, token int64, loop func() (string, time.Time)) {
	k.BusHandler.KeepPresence(ctx, name, ns, token, loop)
	k.record("keeper-done")
	close(k.keeper)
}

func (k *keeperHandler) OnDisconnect(name, ns string, token int64) {
	k.record("disconnect")
	k.BusHandler.OnDisconnect(name, ns, token)
}

func (k *keeperHandler) OnHeartbeat(name, ns string, hb *mitev1.Heartbeat) {
	if k.block {
		k.inside <- struct{}{}
		<-k.release
	}
	k.BusHandler.OnHeartbeat(name, ns, hb)
}

// openSilentStream opens one mite stream, sends its hello, and returns the
// stream with the function that ends it.
func openSilentStream(t *testing.T, addr string, gconnFor func() mitev1.MiteClient, name, ns string) (mitev1.Mite_CommandStreamClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := gconnFor().CommandStream(ctx)
	if err != nil {
		cancel()
		t.Fatalf("command stream to %s: %v", addr, err)
	}
	if err := stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_Hello{
		Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "v1"},
	}}); err != nil {
		cancel()
		t.Fatalf("send hello: %v", err)
	}
	return stream, cancel
}

// TestGatewayIntegration_SilentStreamKeepsPresence opens a stream that
// sends nothing after its hello. The gateway keeps writing the record with
// the connect heartbeat. When the stream ends, the timer stops before
// OnDisconnect, and no write follows the disconnect delete.
func TestGatewayIntegration_SilentStreamKeepsPresence(t *testing.T) {
	natsSrv := startNATS(t)
	var keeper *keeperHandler
	busConn, lis, certAuth := startGatewayWrapped(t, natsSrv.ClientURL(),
		func(h *BusHandler) StreamHandler { keeper = newKeeperHandler(h, false); return keeper },
		func(h *BusHandler) { h.presenceRefreshEvery = 20 * time.Millisecond })
	kv, err := busConn.EnsureKV(bus.KVPresenceBucket, bus.PresenceTTL)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}
	const ns, name = "ns", "silent"
	addr := lis.Addr().String()
	client := func() mitev1.MiteClient { return mitev1.NewMiteClient(dialWithMiteCert(t, addr, certAuth, name, ns)) }
	_, end := openSilentStream(t, addr, client, name, ns)

	var first bus.Presence
	var firstRev uint64
	waitFor(t, func() bool {
		p, rev, found := readPresenceRecord(t, kv, name)
		first, firstRev = p, rev
		return found
	}, 10*time.Second, "no presence record after the hello")
	waitFor(t, func() bool {
		_, rev, _ := readPresenceRecord(t, kv, name)
		return rev >= firstRev+2
	}, 10*time.Second, "the timer did not write the record of the silent stream again")
	p, _, _ := readPresenceRecord(t, kv, name)
	if !p.StreamOpen || !p.LastHeartbeat.Equal(first.LastHeartbeat) {
		t.Errorf("record = %+v, want StreamOpen with the connect heartbeat %s", p, first.LastHeartbeat)
	}

	end()
	waitFor(t, func() bool { return len(keeper.recorded()) == 2 }, 10*time.Second, "the stream did not tear down")
	if got := keeper.recorded(); got[0] != "keeper-done" || got[1] != "disconnect" {
		t.Fatalf("teardown order = %v, want the timer stop before OnDisconnect", got)
	}
	waitFor(t, func() bool {
		_, _, found := readPresenceRecord(t, kv, name)
		return !found
	}, 10*time.Second, "OnDisconnect did not delete the presence record")
}

// TestGatewayIntegration_PresenceTimerEndsWithTheStream holds the read
// loop inside a handler. The timer reports the silent stream with the
// message kind of that handler, in the log and in the presence record.
// When the peer goes away, gRPC ends the stream context, and the timer
// stops while the handler still blocks. OnDisconnect has not run then, so
// a gateway that lost the stream stops its writes by the stream context
// alone, and the record ages out.
func TestGatewayIntegration_PresenceTimerEndsWithTheStream(t *testing.T) {
	natsSrv := startNATS(t)
	var out lockedBuffer
	var keeper *keeperHandler
	busConn, lis, certAuth := startGatewayWrapped(t, natsSrv.ClientURL(),
		func(h *BusHandler) StreamHandler { keeper = newKeeperHandler(h, true); return keeper },
		func(h *BusHandler) {
			h.presenceRefreshEvery = 20 * time.Millisecond
			h.presenceSilentAfter = 50 * time.Millisecond
			h.Logger = slog.New(slog.NewTextHandler(&out, nil))
		})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(keeper.release) }) }
	t.Cleanup(release)

	const ns, name = "ns", "stuck"
	addr := lis.Addr().String()
	gconn := dialWithMiteCert(t, addr, certAuth, name, ns)
	stream, _ := openSilentStream(t, addr, func() mitev1.MiteClient { return mitev1.NewMiteClient(gconn) }, name, ns)
	if err := stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_Heartbeat{
		Heartbeat: &mitev1.Heartbeat{Version: "v1"},
	}}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	select {
	case <-keeper.inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the read loop did not enter the heartbeat handler")
	}

	waitFor(t, func() bool {
		return strings.Contains(out.String(), "readLoop=heartbeat")
	}, 10*time.Second, "no WARN line that names the blocked heartbeat handler")
	kv, err := busConn.EnsureKV(bus.KVPresenceBucket, bus.PresenceTTL)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}
	waitFor(t, func() bool {
		p, _, found := readPresenceRecord(t, kv, name)
		return found && p.StreamOpen && p.ReadLoop == "heartbeat" && !p.ReadLoopSince.IsZero()
	}, 10*time.Second, "the presence record does not name the blocked heartbeat handler")

	_ = gconn.Close()
	select {
	case <-keeper.keeper:
	case <-time.After(10 * time.Second):
		t.Fatal("the presence timer did not stop when the peer went away")
	}
	if got := keeper.recorded(); len(got) != 1 || got[0] != "keeper-done" {
		t.Fatalf("events = %v, want the timer stop while the handler still blocks", got)
	}

	release()
	waitFor(t, func() bool { return len(keeper.recorded()) == 2 }, 10*time.Second, "OnDisconnect did not run after the release")
}
