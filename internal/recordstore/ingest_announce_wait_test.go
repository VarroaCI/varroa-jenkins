package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// announcePod sets the current pod of the controller's placement row, as
// the operator's announce does.
func announcePod(t *testing.T, db *sql.DB, instance string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET cell_instance = $1 WHERE tenant = $2`,
		instance, testTenant); err != nil {
		t.Fatalf("announce the pod: %v", err)
	}
}

// listDeadPod adds instance to the dead_instances list of the controller's
// placement row, as the operator reports a replaced pod.
func listDeadPod(t *testing.T, db *sql.DB, instance string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements
		   SET dead_instances = dead_instances || jsonb_build_array(jsonb_build_object('instance', $1::text))
		 WHERE tenant = $2
	`, instance, testTenant); err != nil {
		t.Fatalf("list the dead pod: %v", err)
	}
}

// applyDeliveryFromPod applies one envelope of kind for runID from sender
// through ApplyDelivery with waitAnnounce true, and answers its error.
func applyDeliveryFromPod(t *testing.T, g *Ingester, runID uuid.UUID, intentKey, kind, sender string, payload any) (ApplyResult, error) {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, kind, payload)
	env.CellInstance = sender
	return g.ApplyDelivery(context.Background(), env, true)
}

// TestOwnerLookupAnswersNotReadyBeforeTheAnnounce proves the not-ready
// answer of the ledger owner lookup. Only the replacement pod of a
// controller with no cell role, before the operator announces it, gets
// it. The answer still names the stored owner.
func TestOwnerLookupAnswersNotReadyBeforeTheAnnounce(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		generation   int64
		placement    string
		dead         string
		cell         string
		instance     string
		wantOwner    string
		wantNotReady bool
	}{
		{name: "the placement names the replaced pod", placement: replacedPod, cell: "cell-a",
			instance: replacementPod, wantOwner: replacedPod, wantNotReady: true},
		{name: "the placement names no pod", placement: "", dead: replacedPod, cell: "cell-a",
			instance: replacementPod, wantOwner: replacedPod, wantNotReady: true},
		{name: "the placement names the sender", placement: replacementPod, cell: "cell-a",
			instance: replacementPod, wantOwner: replacementPod},
		{name: "the placement names another pod", placement: "pod-other", cell: "cell-a",
			instance: replacementPod, wantOwner: replacedPod},
		{name: "the placement lists the sender dead", placement: "", dead: replacementPod, cell: "cell-a",
			instance: replacementPod, wantOwner: replacedPod},
		{name: "the stored owner", placement: replacedPod, cell: "cell-a",
			instance: replacedPod, wantOwner: replacedPod},
		{name: "another controller", placement: replacedPod, cell: "cell-b",
			instance: replacementPod, wantOwner: replacedPod},
		{name: "no caller", placement: replacedPod, wantOwner: replacedPod},
		{name: "a cell run", generation: cellRunGeneration, placement: replacedPod, cell: "cell-a",
			instance: replacementPod, wantOwner: replacedPod},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool, db := setupLedgerPool(t)
			runID, _ := seedOwnedRun(t, pool, replacedPod, tc.generation)
			seedControllerPlacement(t, db, tc.generation, tc.placement)
			if tc.dead != "" {
				listDeadPod(t, db, tc.dead)
			}
			req := ownerLookupRequest{RunID: runID.String()}
			if tc.cell != "" {
				req.Cluster, req.Namespace, req.Cell, req.Instance = testCluster, testNamespace, tc.cell, tc.instance
			}

			resp, err := lookupOwner(context.Background(), pool, req)
			if err != nil {
				t.Fatalf("lookupOwner: %v", err)
			}
			if !resp.Found || resp.Instance != tc.wantOwner || resp.NotReady != tc.wantNotReady {
				t.Fatalf("answer = %+v, want owner %q and NotReady %v", resp, tc.wantOwner, tc.wantNotReady)
			}
		})
	}
}

// TestReplacementPodEventsWaitForTheAnnounce proves the wait of the
// ingest path. Before the announce, a completion and each running fact
// of the replacement pod answer ErrAwaitingAnnounce and write nothing. A
// heartbeat does not wait. After the announce, the same delivery takes
// the run over and gives the real result.
func TestReplacementPodEventsWaitForTheAnnounce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, replacedPod, 0)
	seedControllerPlacement(t, db, 0, replacedPod)
	g := NewIngester(pool, nil, nil)
	completion := RunCompletedPayload{Result: strPtr("SUCCESS")}

	if _, err := applyDeliveryFromPod(t, g, runID, intentKey, KindRunCompleted, replacementPod, completion); !errors.Is(err, ErrAwaitingAnnounce) {
		t.Fatalf("completion before the announce: err = %v, want ErrAwaitingAnnounce", err)
	}
	for _, env := range runningFactEnvelopes(t, runID, intentKey, replacementPod, "resumed", 5) {
		if _, err := g.ApplyDelivery(context.Background(), env, true); !errors.Is(err, ErrAwaitingAnnounce) {
			t.Fatalf("%s before the announce: err = %v, want ErrAwaitingAnnounce", env.Kind, err)
		}
	}
	if _, err := applyDeliveryFromPod(t, g, runID, intentKey, KindRunHeartbeat, replacementPod, map[string]any{"seq": 1}); err != nil {
		t.Fatalf("heartbeat before the announce: err = %v, want none", err)
	}
	s := readReplacementRunState(t, db, runID)
	if s.status != StatusRunning || s.heartbeat || s.ownerCompletion || s.foreignCompletion {
		t.Fatalf("run before the announce = %+v, want RUNNING with nothing written", s)
	}

	announcePod(t, db, replacementPod)
	for _, env := range runningFactEnvelopes(t, runID, intentKey, replacementPod, "resumed", 5) {
		if _, err := g.ApplyDelivery(context.Background(), env, true); err != nil {
			t.Fatalf("%s after the announce: %v", env.Kind, err)
		}
	}
	assertRunningFacts(t, db, runID, "resumed")
	res, err := applyDeliveryFromPod(t, g, runID, intentKey, KindRunCompleted, replacementPod, completion)
	if err != nil || !res.CompletedOwner {
		t.Fatalf("completion after the announce: result %+v, err %v, want the owner completion", res, err)
	}
	if s := readReplacementRunState(t, db, runID); s.result != "SUCCESS" || s.foreignCompletion {
		t.Fatalf("run after the announce = %+v, want SUCCESS from an owner completion", s)
	}
	if owner := intentOwnerOf(t, db, runID); owner != replacementPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacementPod)
	}
}

// TestEventsThatDoNotWaitForTheAnnounce proves the limits of the wait. A
// delivery past the wait (waitAnnounce false) takes the rule of any other
// sender, so the completion is foreign. A late event of a replaced pod
// does not wait: the placement row lists that pod as dead.
func TestEventsThatDoNotWaitForTheAnnounce(t *testing.T) {
	t.Parallel()

	t.Run("past the wait", func(t *testing.T) {
		t.Parallel()

		pool, db := setupLedgerPool(t)
		runID, intentKey := seedOwnedRun(t, pool, replacedPod, 0)
		seedControllerPlacement(t, db, 0, replacedPod)

		res := applyFromPod(t, NewIngester(pool, nil, nil), runID, intentKey, KindRunCompleted, "cell-a", replacementPod,
			RunCompletedPayload{Result: strPtr("SUCCESS")})

		if res.CompletedOwner {
			t.Fatalf("result = %+v, want a foreign completion", res)
		}
		if s := readReplacementRunState(t, db, runID); s.status != StatusRunning || !s.foreignCompletion {
			t.Fatalf("run = %+v, want RUNNING with a foreign completion", s)
		}
	})

	t.Run("a replaced pod after the takeover", func(t *testing.T) {
		t.Parallel()

		pool, db := setupLedgerPool(t)
		runID, intentKey := seedReplacedControllerRun(t, pool, db, 0)
		listDeadPod(t, db, replacedPod)
		g := NewIngester(pool, nil, nil)
		applyFromPod(t, g, runID, intentKey, KindRunHeartbeat, "cell-a", replacementPod, map[string]any{"seq": 1})

		res, err := applyDeliveryFromPod(t, g, runID, intentKey, KindRunCompleted, replacedPod,
			RunCompletedPayload{Result: strPtr("FAILURE")})

		if err != nil || res.CompletedOwner {
			t.Fatalf("late completion: result %+v, err %v, want a foreign completion and no wait", res, err)
		}
	})
}

// testDeliveryMsg returns a bound JetStream message whose metadata names
// delivered as its delivery count.
func testDeliveryMsg(delivered uint64) *nats.Msg {
	return &nats.Msg{
		Reply: fmt.Sprintf("$JS.ACK.%s.%s.%d.1.1.%d.0", bus.BuildsStreamName, RecorderConsumerName, delivered, time.Now().UnixNano()),
		Sub:   &nats.Subscription{},
	}
}

// TestAnnounceWaitOpenBoundsTheDeliveries proves the bound of the wait.
// It counts deliveries, not clock time: the last waiting delivery comes
// about announceWait after the first one. A message with no metadata
// does not wait.
func TestAnnounceWaitOpenBoundsTheDeliveries(t *testing.T) {
	t.Parallel()

	last := uint64(announceWait / announceRetryDelay)
	if !announceWaitOpen(testDeliveryMsg(1)) || !announceWaitOpen(testDeliveryMsg(last)) {
		t.Fatalf("deliveries 1 and %d: want the wait open", last)
	}
	if announceWaitOpen(testDeliveryMsg(last + 1)) {
		t.Fatalf("delivery %d: want the wait closed", last+1)
	}
	if announceWaitOpen(&nats.Msg{}) {
		t.Fatal("a message with no metadata: want the wait closed")
	}
}

// holdSignal is a slog handler that closes held when the consumer logs
// the hold of an event of runID for the announce. The consumer logs the
// hold after the rolled-back write and before its negative
// acknowledgement, so the signal proves that the delivery read the
// placement row and wrote nothing.
type holdSignal struct {
	runID string
	once  sync.Once
	held  chan struct{}
}

func (h *holdSignal) Enabled(context.Context, slog.Level) bool { return true }

func (h *holdSignal) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "waits for the announce") {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "runID" && a.Value.String() == h.runID {
			h.once.Do(func() { close(h.held) })
			return false
		}
		return true
	})
	return nil
}

func (h *holdSignal) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *holdSignal) WithGroup(string) slog.Handler { return h }

// TestIngestConsumerHoldsACompletionUntilTheAnnounce proves the consumer
// disposition. The completion of the replacement pod reaches the stream
// before the announce. The test announces the pod only after the
// consumer logs the hold of the first delivery, so that delivery always
// reads the old placement row. The consumer delivers the completion
// again after announceRetryDelay, and that delivery ends the run with
// its real result.
func TestIngestConsumerHoldsACompletionUntilTheAnnounce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, replacedPod, 0)
	seedControllerPlacement(t, db, 0, replacedPod)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sig := &holdSignal{runID: runID.String(), held: make(chan struct{})}
	go func() {
		_ = RunIngestConsumer(ctx, conn, NewIngester(pool, conn, nil), nil, 24*time.Hour, slog.New(sig))
	}()

	env := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: strPtr("SUCCESS")})
	env.CellInstance = replacementPod
	publishBuildEvent(t, conn, env)

	delivered := func() uint64 {
		info, err := conn.JetStream().ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		return info.Delivered.Consumer
	}
	select {
	case <-sig.held:
	case <-time.After(waitForRowTimeout):
		t.Fatal("the consumer did not hold the first delivery for the announce")
	}
	if s := readReplacementRunState(t, db, runID); s.status != StatusRunning || s.foreignCompletion || s.ownerCompletion {
		t.Fatalf("run after the held delivery = %+v, want RUNNING with no completion stored", s)
	}

	announcePod(t, db, replacementPod)
	deadline := time.Now().Add(announceRetryDelay + 5*time.Second)
	for {
		if s := readReplacementRunState(t, db, runID); s.result == "SUCCESS" && s.ownerCompletion {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run = %+v, want SUCCESS from the delivery after the announce", readReplacementRunState(t, db, runID))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := delivered(); got < 2 {
		t.Fatalf("deliveries = %d, want a second delivery after the wait", got)
	}
}
