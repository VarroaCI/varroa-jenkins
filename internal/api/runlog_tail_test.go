package api

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// startRunLogTestNATS starts an embedded, JetStream-less NATS server for
// one test, torn down on cleanup.
func startRunLogTestNATS(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// fakeTailGateway answers every request on its cell's tail-control
// subject with {"ok":true,"leaseSeconds":90}, counting start and stop
// requests separately.
type fakeTailGateway struct {
	starts, stops int32
}

func (g *fakeTailGateway) subscribe(t *testing.T, conn *bus.Conn, ns, name string) {
	const cluster = "core"
	t.Helper()
	sub, err := conn.SubscribeRequest(bus.TailSubject(cluster, ns, name), "gw", func(data []byte) []byte {
		var req runTailRequest
		_ = json.Unmarshal(data, &req)
		switch req.Action {
		case "start":
			atomic.AddInt32(&g.starts, 1)
		case "stop":
			atomic.AddInt32(&g.stops, 1)
		}
		reply, _ := json.Marshal(runTailReply{OK: true, LeaseSeconds: 90})
		return reply
	})
	if err != nil {
		t.Fatalf("subscribe tail control: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// --- 5.5: runTailRegistry ---

// TestRunTailRegistry_SharedLeaseAndReferenceCount opens three viewers
// of the same run, asserts one subscription and one start request per
// renewal period, then closes all three and asserts one stop.
func TestRunTailRegistry_SharedLeaseAndReferenceCount(t *testing.T) {
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, name, runID = "core", "ns1", "cell1", "run-tail-1"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, name)

	reg := newRunTailRegistry(conn)

	var releases []func()
	for i := 0; i < 3; i++ {
		_, release, attachErr := reg.attach(cluster, ns, name, runID, "folder/job", 7)
		if attachErr != nil {
			t.Fatalf("attach viewer %d: %v", i, attachErr)
		}
		releases = append(releases, release)
	}

	waitForCondition(t, func() bool { return atomic.LoadInt32(&gw.starts) == 1 }, "expected exactly one start request for three viewers")

	reg.mu.Lock()
	entryCount := len(reg.entries)
	var viewerCount int
	if e, ok := reg.entries[runID]; ok {
		viewerCount = len(e.viewers)
	}
	reg.mu.Unlock()
	if entryCount != 1 {
		t.Fatalf("expected one shared entry, got %d", entryCount)
	}
	if viewerCount != 3 {
		t.Fatalf("expected three registered viewers, got %d", viewerCount)
	}

	for _, release := range releases {
		release()
	}

	waitForCondition(t, func() bool { return atomic.LoadInt32(&gw.stops) == 1 }, "expected exactly one stop request once every viewer released")

	reg.mu.Lock()
	remaining := len(reg.entries)
	reg.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected the entry to be removed, got %d remaining", remaining)
	}
}

// TestRunTailRegistry_RenewsOnASchedule proves the registry resends the
// start action every renewal period while a viewer stays connected.
func TestRunTailRegistry_RenewsOnASchedule(t *testing.T) {
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, name, runID = "core", "ns1", "cell1", "run-tail-2"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, name)

	reg := newRunTailRegistry(conn)
	reg.renewOverride = 30 * time.Millisecond // shrink the 30s design default for the test

	_, release, err := reg.attach(cluster, ns, name, runID, "folder/job", 7)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(release)

	waitForCondition(t, func() bool { return atomic.LoadInt32(&gw.starts) >= 3 }, "expected renewals to keep resending the start action")
}

// TestRunTailRegistry_BroadcastDoesNotRaceDetach proves a NATS-callback
// broadcast never sends on a viewer channel a concurrent release() has
// already closed. A persistent viewer keeps the run's subscription
// alive while a publisher floods the run's log subject and, in
// parallel, transient viewers attach and immediately release — the
// shape that races broadcast's snapshot against detachFunc's close. A
// send on a closed channel panics the whole test binary, so this test
// simply not panicking is the proof.
func TestRunTailRegistry_BroadcastDoesNotRaceDetach(t *testing.T) {
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, name, runID = "core", "ns1", "cell-race", "run-tail-race"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, name)

	reg := newRunTailRegistry(conn)

	// A persistent viewer keeps the entry, and its NATS subscription,
	// alive for the whole test.
	_, keepAlive, err := reg.attach(cluster, ns, name, runID, "folder/job", 1)
	if err != nil {
		t.Fatalf("attach persistent viewer: %v", err)
	}
	t.Cleanup(keepAlive)

	stopPublish := make(chan struct{})
	var publishers sync.WaitGroup
	subject := bus.LogSubject(cluster, ns, name, runID)
	for p := 0; p < 4; p++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for {
				select {
				case <-stopPublish:
					return
				default:
					_ = conn.Publish(subject, []byte("line"))
				}
			}
		}()
	}
	t.Cleanup(func() {
		close(stopPublish)
		publishers.Wait()
	})

	const iterations = 3000
	for i := 0; i < iterations; i++ {
		_, release, attachErr := reg.attach(cluster, ns, name, runID, "folder/job", 1)
		if attachErr != nil {
			t.Fatalf("attach transient viewer %d: %v", i, attachErr)
		}
		release()
	}
}

func waitForCondition(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
