package recordstore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestDuplicateBackstopMarksSecondLiveRun asserts a second live run of
// one revision is marked DUPLICATE and publishes one cancel_run action.
func TestDuplicateBackstopMarksSecondLiveRun(t *testing.T) {
	pool, db := setupLedgerPool(t)

	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan []byte, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	g := NewIngester(pool, conn, nil)

	revision := "abc123"
	runA := seedGrantedIntent(t, pool, "manual:dup-a")
	buildA := int64(1)
	startedA := newTestEnvelope(t, runA, "manual:dup-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildA, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedA); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}

	runB := seedGrantedIntent(t, pool, "manual:dup-b")
	buildB := int64(2)
	startedB := newTestEnvelope(t, runB, "manual:dup-b", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildB, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedB); err != nil {
		t.Fatalf("apply run.started B: %v", err)
	}

	// runB is the newer of the two (later UUIDv7 timestamp), so it is
	// the one marked DUPLICATE.
	var statusA, statusB string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runA).Scan(&statusA); err != nil {
		t.Fatalf("read run A: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusA != StatusRunning {
		t.Fatalf("run A status = %q, want RUNNING", statusA)
	}
	if statusB != StatusDuplicate {
		t.Fatalf("run B status = %q, want DUPLICATE", statusB)
	}

	select {
	case data := <-received:
		var action LedgerAction
		if err := json.Unmarshal(data, &action); err != nil {
			t.Fatalf("unmarshal action: %v", err)
		}
		if action.Kind != LedgerActionCancelRun {
			t.Fatalf("kind = %q, want cancel_run", action.Kind)
		}
		if action.RunID != runB.String() {
			t.Fatalf("run_id = %q, want %q", action.RunID, runB.String())
		}
		if action.Actor != LedgerActionRecorder {
			t.Fatalf("actor = %q, want %q", action.Actor, LedgerActionRecorder)
		}
	default:
		t.Fatal("no cancel_run action was published")
	}
}

// TestDuplicateBackstopFinishedArrivingRunIsLeftAlone asserts run.completed
// arriving before run.started for a revision a RUNNING run also holds
// leaves the arriving run COMPLETED and publishes no action.
func TestDuplicateBackstopFinishedArrivingRunIsLeftAlone(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	revision := "rev-finished"
	runLive := seedGrantedIntent(t, pool, "manual:dup-live")
	buildLive := int64(1)
	startedLive := newTestEnvelope(t, runLive, "manual:dup-live", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildLive, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedLive); err != nil {
		t.Fatalf("apply run.started live: %v", err)
	}

	runFinished := seedGrantedIntent(t, pool, "manual:dup-finished")
	result := "SUCCESS"
	completedFirst := newTestEnvelope(t, runFinished, "manual:dup-finished", KindRunCompleted, RunCompletedPayload{Result: &result})
	if _, err := g.Apply(context.Background(), completedFirst); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	buildFinished := int64(2)
	startedFinished := newTestEnvelope(t, runFinished, "manual:dup-finished", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildFinished, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedFinished); err != nil {
		t.Fatalf("apply run.started finished: %v", err)
	}

	var statusFinished, statusLive string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runFinished).Scan(&statusFinished); err != nil {
		t.Fatalf("read finished run: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runLive).Scan(&statusLive); err != nil {
		t.Fatalf("read live run: %v", err)
	}
	if statusFinished != StatusCompleted {
		t.Fatalf("finished run status = %q, want COMPLETED", statusFinished)
	}
	if statusLive != StatusRunning {
		t.Fatalf("live run status = %q, want RUNNING (untouched)", statusLive)
	}
}

// TestDuplicateBackstopFinishedRevisionIsNotADuplicate asserts a
// run.started reporting a revision only a COMPLETED run holds keeps the
// new run RUNNING and publishes no action.
func TestDuplicateBackstopFinishedRevisionIsNotADuplicate(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	revision := "rev-old"
	runOld := seedGrantedIntent(t, pool, "manual:dup-old")
	buildOld := int64(1)
	startedOld := newTestEnvelope(t, runOld, "manual:dup-old", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildOld, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedOld); err != nil {
		t.Fatalf("apply run.started old: %v", err)
	}
	result := "SUCCESS"
	completedOld := newTestEnvelope(t, runOld, "manual:dup-old", KindRunCompleted, RunCompletedPayload{Result: &result})
	if _, err := g.Apply(context.Background(), completedOld); err != nil {
		t.Fatalf("apply run.completed old: %v", err)
	}

	runNew := seedGrantedIntent(t, pool, "manual:dup-new")
	buildNew := int64(2)
	startedNew := newTestEnvelope(t, runNew, "manual:dup-new", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNew, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedNew); err != nil {
		t.Fatalf("apply run.started new: %v", err)
	}

	var statusNew string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runNew).Scan(&statusNew); err != nil {
		t.Fatalf("read new run: %v", err)
	}
	if statusNew != StatusRunning {
		t.Fatalf("new run status = %q, want RUNNING", statusNew)
	}
}

// TestDuplicateBackstopSkipsEmptyRevision asserts an empty scm_revision
// never triggers the backstop, even for two live runs of the same job.
func TestDuplicateBackstopSkipsEmptyRevision(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runA := seedGrantedIntent(t, pool, "manual:dup-empty-a")
	buildA := int64(1)
	startedA := newTestEnvelope(t, runA, "manual:dup-empty-a", KindRunStarted, RunStartedPayload{BuildNumber: &buildA})
	if _, err := g.Apply(context.Background(), startedA); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}

	runB := seedGrantedIntent(t, pool, "manual:dup-empty-b")
	buildB := int64(2)
	startedB := newTestEnvelope(t, runB, "manual:dup-empty-b", KindRunStarted, RunStartedPayload{BuildNumber: &buildB})
	if _, err := g.Apply(context.Background(), startedB); err != nil {
		t.Fatalf("apply run.started B: %v", err)
	}

	var statusA, statusB string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runA).Scan(&statusA); err != nil {
		t.Fatalf("read run A: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusA != StatusRunning || statusB != StatusRunning {
		t.Fatalf("statuses = %q, %q, want both RUNNING", statusA, statusB)
	}
}

// TestDuplicateStaysTerminalAfterLaterCompletion asserts a later
// run.completed for the marked run leaves its status at DUPLICATE.
func TestDuplicateStaysTerminalAfterLaterCompletion(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	revision := "rev-terminal"
	runA := seedGrantedIntent(t, pool, "manual:dup-term-a")
	buildA := int64(1)
	startedA := newTestEnvelope(t, runA, "manual:dup-term-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildA, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedA); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}
	runB := seedGrantedIntent(t, pool, "manual:dup-term-b")
	buildB := int64(2)
	startedB := newTestEnvelope(t, runB, "manual:dup-term-b", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildB, SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedB); err != nil {
		t.Fatalf("apply run.started B: %v", err)
	}

	var statusB string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusB != StatusDuplicate {
		t.Fatalf("run B status = %q, want DUPLICATE before the completion", statusB)
	}

	result := "ABORTED"
	completedB := newTestEnvelope(t, runB, "manual:dup-term-b", KindRunCompleted, RunCompletedPayload{Result: &result})
	if _, err := g.Apply(context.Background(), completedB); err != nil {
		t.Fatalf("apply run.completed B: %v", err)
	}

	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B after completion: %v", err)
	}
	if statusB != StatusDuplicate {
		t.Fatalf("run B status = %q, want DUPLICATE to stay terminal", statusB)
	}
}

// TestDuplicateBackstopConcurrentStarts asserts two concurrent
// run.started writes for one revision produce exactly one DUPLICATE row
// and one published action.
func TestDuplicateBackstopConcurrentStarts(t *testing.T) {
	pool, db := setupLedgerPool(t)

	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	var mu sync.Mutex
	var received [][]byte
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		mu.Lock()
		received = append(received, msg.Data)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Two separate Ingester instances stand in for two recorder replicas
	// sharing one pool.
	gatewayA := NewIngester(pool, conn, nil)
	gatewayB := NewIngester(pool, conn, nil)

	revision := "rev-concurrent"
	runA := seedGrantedIntent(t, pool, "manual:dup-conc-a")
	runB := seedGrantedIntent(t, pool, "manual:dup-conc-b")
	buildA := int64(1)
	buildB := int64(2)
	startedA := newTestEnvelope(t, runA, "manual:dup-conc-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildA, SCMRevision: &revision,
	})
	startedB := newTestEnvelope(t, runB, "manual:dup-conc-b", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildB, SCMRevision: &revision,
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := gatewayA.Apply(context.Background(), startedA); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := gatewayB.Apply(context.Background(), startedB); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("apply concurrent run.started: %v", err)
	}

	var duplicateCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.runs WHERE run_id IN ($1, $2) AND status = 'DUPLICATE'
	`, runA, runB).Scan(&duplicateCount); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if duplicateCount != 1 {
		t.Fatalf("duplicate count = %d, want 1", duplicateCount)
	}

	// Wait briefly for the async subscriber callback.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d actions, want 1", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startDuplicateTestServer starts an embedded NATS server for the
// duplicate-backstop publish tests.
func startDuplicateTestServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Port:     -1,
		StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestDuplicateBackstopMatchesRevisionRunSourceWrote asserts a
// run.started carrying a claim revision stops a live run whose revision
// a run.source event wrote.
func TestDuplicateBackstopMatchesRevisionRunSourceWrote(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	revision := "rev-from-source"
	runA := seedGrantedIntent(t, pool, "manual:dup-source-a")
	buildA := int64(1)
	startedA := newTestEnvelope(t, runA, "manual:dup-source-a", KindRunStarted, RunStartedPayload{BuildNumber: &buildA})
	if _, err := g.Apply(context.Background(), startedA); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}
	sourceA := newTestEnvelope(t, runA, "manual:dup-source-a", KindRunSource, RunSourcePayload{
		Seq:         1,
		SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), sourceA); err != nil {
		t.Fatalf("apply run.source A: %v", err)
	}

	runB := seedGrantedIntent(t, pool, "manual:dup-source-b")
	buildB := int64(2)
	startedB := newTestEnvelope(t, runB, "manual:dup-source-b", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildB,
		SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedB); err != nil {
		t.Fatalf("apply run.started B: %v", err)
	}

	// runB is the newer of the two, so it is the one marked DUPLICATE.
	var statusA, statusB string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runA).Scan(&statusA); err != nil {
		t.Fatalf("read run A: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusA != StatusRunning {
		t.Fatalf("run A status = %q, want RUNNING", statusA)
	}
	if statusB != StatusDuplicate {
		t.Fatalf("run B status = %q, want DUPLICATE", statusB)
	}
}

// TestApplyRunSourceRaisesNoDuplicateBackstop asserts a run.source write
// that reports a revision a live run holds marks no run and publishes
// no action.
func TestApplyRunSourceRaisesNoDuplicateBackstop(t *testing.T) {
	pool, db := setupLedgerPool(t)

	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan []byte, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	g := NewIngester(pool, conn, nil)

	revision := "rev-live-source"
	runA := seedGrantedIntent(t, pool, "manual:dup-source-live")
	buildA := int64(1)
	startedA := newTestEnvelope(t, runA, "manual:dup-source-live", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildA,
		SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), startedA); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}

	runB := seedGrantedIntent(t, pool, "manual:dup-source-arriving")
	sourceB := newTestEnvelope(t, runB, "manual:dup-source-arriving", KindRunSource, RunSourcePayload{
		Seq:         0,
		SCMRevision: &revision,
	})
	if _, err := g.Apply(context.Background(), sourceB); err != nil {
		t.Fatalf("apply run.source B: %v", err)
	}

	var statusA, statusB string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runA).Scan(&statusA); err != nil {
		t.Fatalf("read run A: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runB).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusA != StatusRunning {
		t.Fatalf("run A status = %q, want RUNNING", statusA)
	}
	if statusB != "QUEUED" {
		t.Fatalf("run B status = %q, want QUEUED", statusB)
	}

	time.Sleep(200 * time.Millisecond)
	select {
	case <-received:
		t.Fatal("run.source published a ledger action")
	default:
	}
}
