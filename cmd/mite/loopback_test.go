package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// startTestLoopback starts a real loopback listener on an OS-chosen port, so
// tests never collide on 9095. It cleans the listener up via ctx cancellation.
func startTestLoopback(t *testing.T) (*Agent, int) {
	t.Helper()
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackTokenDir = t.TempDir()
	agent.loopbackPort = 0 // ask the OS for a free port
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := agent.startLoopback(ctx); err != nil {
		t.Fatalf("startLoopback: %v", err)
	}
	port := agent.loopbackListener.Addr().(*net.TCPAddr).Port
	return agent, port
}

// --- 4.1: listener bind + oversized body ---

func TestLoopbackListenerBindsLoopbackOnly(t *testing.T) {
	agent, port := startTestLoopback(t)
	addr := agent.loopbackListener.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("expected listener bound to 127.0.0.1, got %s", addr)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/events", port)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to the bound loopback address failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 for an empty batch, got %d", resp.StatusCode)
	}
}

// oversizedEventsBody is syntactically valid JSON well past 1 MiB: a large
// array of small BuildEvent objects. The decoder must keep reading through
// the array (rather than fail fast on a syntax error) to exercise the
// MaxBytesReader cap, the way a real oversized batch would.
func oversizedEventsBody() string {
	var b strings.Builder
	b.WriteByte('[')
	one := newBuildEventJSON(1, "STAGE")
	for b.Len() < loopbackMaxBodyBytes+1 {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(one)
	}
	b.WriteByte(']')
	return b.String()
}

func TestLoopbackOversizedBodyRejected413(t *testing.T) {
	agent, port := startTestLoopback(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/events", port)
	oversized := strings.NewReader(oversizedEventsBody())
	req, _ := http.NewRequest(http.MethodPost, url, oversized)
	req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for an oversized body, got %d", resp.StatusCode)
	}
}

// --- 4.2: token write order and auth ---

func TestLoopbackTokenWrittenBeforeListenerOpens(t *testing.T) {
	agent, _ := startTestLoopback(t)

	if len(agent.loopbackToken) != 43 { // 32 random bytes, base64url, no padding
		t.Errorf("expected a 43-char base64url token, got %d chars: %q", len(agent.loopbackToken), agent.loopbackToken)
	}

	path := filepath.Join(agent.loopbackTokenDir, loopbackTokenFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token file not written before startLoopback returned: %v", err)
	}
	if string(data) != agent.loopbackToken {
		t.Errorf("token file content mismatch: got %q want %q", data, agent.loopbackToken)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("expected file mode 0644, got %v", info.Mode().Perm())
	}
}

func TestLoopbackAuthAcceptsFileToken(t *testing.T) {
	agent, port := startTestLoopback(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/events", port)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 for the file token, got %d", resp.StatusCode)
	}
}

func TestLoopbackAuthRejectsWrongToken(t *testing.T) {
	_, port := startTestLoopback(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/events", port)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for a wrong token, got %d", resp.StatusCode)
	}
}

func TestLoopbackAuthRejectsMissingBearer(t *testing.T) {
	_, port := startTestLoopback(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/events", port)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("[]"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for a missing bearer, got %d", resp.StatusCode)
	}
}

// --- 4.3 / 4.3a: events, retention, replay ---

func newBuildEventJSON(seq int64, phase string) string {
	return fmt.Sprintf(`{"phase":%q,"run_id":"r1","job_path":"j","seq":%d,"build_number":1,"emitted_at_millis":1}`,
		phase, seq)
}

func TestEventsAcceptedBatchForwardsAndReturnsAcks(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	var sent []*mitev1.BuildEvent
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = append(sent, m.GetBuildEvent())
		return nil
	}})
	// A pre-buffered ack proves the response drains what processCommands
	// collected since the previous call.
	agent.eventAcks.push(&mitev1.EventAck{RunId: "r1", EventSeq: 1, Status: ackStatusStored})

	body := "[" + strings.Join([]string{
		newBuildEventJSON(1, "STARTED"),
		newBuildEventJSON(2, "STAGE"),
		newBuildEventJSON(3, "COMPLETED"),
	}, ",") + "]"

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(sent) != 3 {
		t.Fatalf("expected 3 forwarded events, got %d", len(sent))
	}
	var acks []*mitev1.EventAck
	if err := json.Unmarshal(rec.Body.Bytes(), &acks); err != nil {
		t.Fatalf("decode acks: %v", err)
	}
	if len(acks) != 1 || acks[0].RunId != "r1" {
		t.Errorf("expected the pre-buffered ack in the response, got %+v", acks)
	}
}

func TestEventsDownstreamDown503(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	// The agent holds no session: the stream never connected.

	body := "[" + newBuildEventJSON(1, "STARTED") + "]"
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req) // must not panic on a.conn.Close() with a.conn == nil

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the stream never connected, got %d", rec.Code)
	}
}

func TestEventsRetentionFullRefusesNoDrop(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.eventRetention = newEventRetentionStore(10) // tiny cap
	var sent int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}})

	body := "[" + newBuildEventJSON(1, "STARTED") + "]"
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 at the retention cap, got %d", rec.Code)
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Errorf("expected nothing forwarded at the cap, got %d sends", sent)
	}
	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected no retained event dropped or added at refusal, got %v", got)
	}
}

func TestEventsSendErrorClosesAndAnswers503(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		return fmt.Errorf("boom")
	}})
	// A real, never-dialed ClientConn, so the session's close on a send
	// error acts on a real connection.
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	setTestConn(agent, conn)

	body := "[" + newBuildEventJSON(1, "STARTED") + "]"
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on a send error, got %d", rec.Code)
	}
	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected the failed send rolled back, still retained: %v", got)
	}
}

func TestEventsBadJSONIs400(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader("not-json"))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for undecodable JSON, got %d", rec.Code)
	}
}

func TestEventAckReleasesRetainedEventOnStored(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 5, Phase: "COMPLETED", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	id, _, ok := agent.eventRetention.reserve(retentionKey{runID: evt.RunId, seq: evt.Seq}, size)
	if !ok {
		t.Fatal("reserve failed")
	}
	agent.eventRetention.commit(id, evt, size)

	agent.handleEventAck(context.Background(), &mitev1.EventAck{RunId: "r1", EventSeq: 5, Status: ackStatusStored})

	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected release on STORED, still retained: %v", got)
	}
	if acks := agent.eventAcks.drain(); len(acks) != 1 {
		t.Errorf("expected the ack buffered for the plugin, got %d", len(acks))
	}
}

func TestEventAckReleasesRetainedEventOnRejected(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 7, Phase: "COMPLETED", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	id, _, _ := agent.eventRetention.reserve(retentionKey{runID: evt.RunId, seq: evt.Seq}, size)
	agent.eventRetention.commit(id, evt, size)

	agent.handleEventAck(context.Background(), &mitev1.EventAck{RunId: "r1", EventSeq: 7, Status: ackStatusRejected, Reason: "bad payload"})

	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected release on REJECTED, still retained: %v", got)
	}
}

// TestEventAckBeforeCommitLeavesNoRetainedEntry covers the review finding:
// on a first delivery, event_ack can arrive after reserve opens a
// reservation and before that reservation's own commit runs. handleEventAck
// must record the ack on the still-open reservation rather than discard it,
// so commit retains nothing, refunds the charge, and a reconnect replay
// sends nothing for this key — while the ack still reaches the plugin's
// next /v1/events response.
func TestEventAckBeforeCommitLeavesNoRetainedEntry(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	id, _, ok := agent.eventRetention.reserve(key, size)
	if !ok {
		t.Fatal("reserve failed")
	}

	// The event_ack lands before this handler's own commit call, the exact
	// interleaving the finding describes.
	agent.handleEventAck(context.Background(), &mitev1.EventAck{
		RunId: evt.RunId, EventSeq: evt.Seq, Status: ackStatusStored,
	})

	charge := agent.eventRetention.commit(id, evt, size)
	if charge != 0 {
		t.Errorf("expected commit to report a zero net delta for an already-acked event, got %d", charge)
	}
	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected no retained entry (a replay must send nothing), got %v", got)
	}
	if agent.eventRetention.totalBytes != 0 {
		t.Errorf("expected totalBytes 0, got %d", agent.eventRetention.totalBytes)
	}
	if n := agent.eventRetention.pendingCount(); n != 0 {
		t.Errorf("expected no reservation left open, got %d", n)
	}
	if ok, detail := agent.eventRetention.checkInvariant(); !ok {
		t.Fatalf("invariant broken: %s", detail)
	}
	acks := agent.eventAcks.drain()
	if len(acks) != 1 || acks[0].RunId != evt.RunId || acks[0].EventSeq != evt.Seq {
		t.Fatalf("expected exactly one ack in the buffer for %s/%d, got %v", evt.RunId, evt.Seq, acks)
	}
}

// TestEventAckBeforeRollbackLeavesNoRetainedEntry covers the same
// interleaving, but the handler's send fails after the ack arrives:
// rollback must still leave nothing retained, refund the charge, and the
// ack must still reach the plugin.
func TestEventAckBeforeRollbackLeavesNoRetainedEntry(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	id, _, ok := agent.eventRetention.reserve(key, size)
	if !ok {
		t.Fatal("reserve failed")
	}

	agent.handleEventAck(context.Background(), &mitev1.EventAck{
		RunId: evt.RunId, EventSeq: evt.Seq, Status: ackStatusRejected, Reason: "bad payload",
	})

	charge := agent.eventRetention.rollback(id)
	if charge != 0 {
		t.Errorf("expected rollback to report a zero delta, got %d", charge)
	}
	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected no retained entry (a replay must send nothing), got %v", got)
	}
	if agent.eventRetention.totalBytes != 0 {
		t.Errorf("expected totalBytes 0, got %d", agent.eventRetention.totalBytes)
	}
	if n := agent.eventRetention.pendingCount(); n != 0 {
		t.Errorf("expected no reservation left open, got %d", n)
	}
	acks := agent.eventAcks.drain()
	if len(acks) != 1 || acks[0].RunId != evt.RunId || acks[0].EventSeq != evt.Seq {
		t.Fatalf("expected exactly one ack in the buffer for %s/%d, got %v", evt.RunId, evt.Seq, acks)
	}
}

func TestReplayAfterReconnectOrderedPerRun(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	for _, seq := range []int64{3, 1, 2} { // committed out of arrival order
		evt := &mitev1.BuildEvent{RunId: "r1", Seq: seq, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
		size := eventSize(evt)
		id, _, _ := agent.eventRetention.reserve(retentionKey{runID: evt.RunId, seq: evt.Seq}, size)
		agent.eventRetention.commit(id, evt, size)
	}
	var sentSeqs []int64
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sentSeqs = append(sentSeqs, m.GetBuildEvent().Seq)
		return nil
	}})

	agent.replayRetainedEvents()

	want := []int64{1, 2, 3}
	if len(sentSeqs) != len(want) {
		t.Fatalf("expected %d replayed events, got %d", len(want), len(sentSeqs))
	}
	for i, w := range want {
		if sentSeqs[i] != w {
			t.Errorf("replay order[%d] = %d, want %d (full: %v)", i, sentSeqs[i], w, sentSeqs)
		}
	}
}

func TestReplayWithNoRetainedEventsIsNoop(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.replayRetainedEvents() // no session; must not panic
	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected nothing retained with nothing ever added, got %v", got)
	}
}

// postEvents POSTs one /v1/events batch of the given seq/phase pairs and
// returns the recorded response.
func postEvents(t *testing.T, agent *Agent, events ...string) *httptest.ResponseRecorder {
	t.Helper()
	body := "[" + strings.Join(events, ",") + "]"
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleEvents(rec, req)
	return rec
}

// decodedEventSize decodes json into a BuildEvent and returns the byte size
// the retention store would charge for it, so the expectation is derived the
// same way the production code computes it rather than hand-duplicated.
func decodedEventSize(t *testing.T, jsonBody string) int64 {
	t.Helper()
	var evt mitev1.BuildEvent
	if err := json.Unmarshal([]byte(jsonBody), &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return eventSize(&evt)
}

// TestEventsDuplicateDeliveryKeepsOneEntry covers the review finding: a
// retried delivery of the same (run_id, event_seq) must replace the
// retained entry in place rather than doubling the ordering slice and
// leaking the earlier reservation's bytes.
func TestEventsDuplicateDeliveryKeepsOneEntry(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	var sendCount int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sendCount, 1)
		return nil
	}})

	seq0 := newBuildEventJSON(0, "STARTED")
	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("retried delivery: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	got := agent.eventRetention.snapshotOrdered()
	if len(got) != 1 {
		t.Fatalf("expected one retained entry after a duplicate delivery, got %d: %v", len(got), got)
	}
	if got[0].Seq != 0 {
		t.Errorf("expected the retained entry to be seq 0, got %d", got[0].Seq)
	}
	wantBytes := decodedEventSize(t, seq0)
	if agent.eventRetention.totalBytes != wantBytes {
		t.Errorf("expected totalBytes %d after a duplicate commit, got %d (no leaked reservation)", wantBytes, agent.eventRetention.totalBytes)
	}
	if atomic.LoadInt32(&sendCount) != 2 {
		t.Errorf("expected both deliveries forwarded on the wire, got %d sends", sendCount)
	}
}

// TestEventRetentionReserveDeltaZeroOnIdenticalRetry covers the review
// finding: reserve must charge only the delta against an existing entry, so
// a same-size retry at the cap costs nothing extra to admit.
func TestEventRetentionReserveDeltaZeroOnIdenticalRetry(t *testing.T) {
	store := newEventRetentionStore(1024)
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	id1, delta, ok := store.reserve(key, size)
	if !ok || delta != size {
		t.Fatalf("first reserve: got delta=%d ok=%v, want delta=%d ok=true", delta, ok, size)
	}
	store.commit(id1, evt, size)

	id2, delta, ok := store.reserve(key, size)
	if !ok {
		t.Fatal("retry reserve at an identical size should succeed")
	}
	if delta != 0 {
		t.Errorf("expected delta 0 on an identical-size retry (reserve's own admission charge), got %d", delta)
	}
	store.commit(id2, evt, size)

	if store.totalBytes != size {
		t.Errorf("expected totalBytes unchanged at %d after the retry, got %d", size, store.totalBytes)
	}
}

// TestEventRetentionReserveReleaseCommitInterleaving covers the review
// finding: an event_ack for (run_id, event_seq) can release the entry a
// reserve is about to replace before that reserve's own commit runs.
// commit must still charge for the replacement's bytes rather than letting
// it enter byKey for free, which would understate retention and drive a
// later ack's release negative.
func TestEventRetentionReserveReleaseCommitInterleaving(t *testing.T) {
	store := newEventRetentionStore(1024)
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	// The first delivery lands and commits.
	id1, _, ok := store.reserve(key, size)
	if !ok {
		t.Fatal("first reserve failed")
	}
	store.commit(id1, evt, size)
	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the first commit: %s", detail)
	}

	// A retry of the same delivery reserves the same key at the same size
	// (delta 0), then an event_ack for the original delivery releases it
	// before the retry's send completes.
	id2, _, ok := store.reserve(key, size)
	if !ok {
		t.Fatal("retry reserve failed")
	}
	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the retry reserve: %s", detail)
	}
	freed, released := store.release(evt.RunId, evt.Seq, ackStatusStored)
	if !released || freed != size {
		t.Fatalf("expected release to free %d bytes for the original entry, got %d ok=%v", size, freed, released)
	}
	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the release: %s", detail)
	}

	// The retry's send succeeds and commits. Its own reserve call charged
	// nothing (an identical-size retry, before the release), so commit
	// alone must report the full delta the release left uncharged.
	charge := store.commit(id2, evt, size)
	if charge != size {
		t.Errorf("expected commit to report the full retained-bytes delta %d, got %d", size, charge)
	}

	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the replacement commit: %s", detail)
	}
	if store.totalBytes < 0 {
		t.Fatalf("totalBytes went negative: %d", store.totalBytes)
	}
	if store.totalBytes != size {
		t.Errorf("expected totalBytes %d for the one retained entry, got %d", size, store.totalBytes)
	}
	got := store.snapshotOrdered()
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("expected exactly one retained entry (seq 1), got %v", got)
	}
	if n := store.pendingCount(); n != 0 {
		t.Errorf("expected no reservation left open, got %d", n)
	}
}

// TestEventRetentionReserveReleaseRollbackInterleaving covers the same
// interleaving as above, but the retry's send fails instead of succeeding:
// the already-acked original entry must not resurrect, and totalBytes must
// land at zero rather than negative.
func TestEventRetentionReserveReleaseRollbackInterleaving(t *testing.T) {
	store := newEventRetentionStore(1024)
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	id1, _, ok := store.reserve(key, size)
	if !ok {
		t.Fatal("first reserve failed")
	}
	store.commit(id1, evt, size)

	id2, _, ok := store.reserve(key, size)
	if !ok {
		t.Fatal("retry reserve failed")
	}
	if _, released := store.release(evt.RunId, evt.Seq, ackStatusStored); !released {
		t.Fatal("expected release to find the original entry")
	}
	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the release: %s", detail)
	}

	store.rollback(id2)

	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after the rollback: %s", detail)
	}
	if store.totalBytes != 0 {
		t.Errorf("expected totalBytes 0 (already-acked entry, failed retry), got %d", store.totalBytes)
	}
	if got := store.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected nothing retained, got %v", got)
	}
	if n := store.pendingCount(); n != 0 {
		t.Errorf("expected no reservation left open, got %d", n)
	}
}

// TestEventRetentionSupersededReservationNeverDoublesMetric covers the
// review finding: request A times out on the client side while its handler
// keeps running on the server; retry B reserves the same key and supersedes
// A's reservation before A commits. A's own commit or rollback call, once
// it finally lands, must contribute nothing to the retained-bytes metric —
// its charge already moved when B's reserve superseded it — so the two
// overlapping handlers never both add a charge for the same bytes.
//
// It runs every combination of commit or rollback for A and B, in both
// resolution orders, and after each checks that the sum of every metric
// delta commit and rollback returned equals totalBytes. The first case
// below, "A commits then B commits, A resolves first", is the exact
// scenario the finding describes.
func TestEventRetentionSupersededReservationNeverDoublesMetric(t *testing.T) {
	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 1, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	key := retentionKey{runID: evt.RunId, seq: evt.Seq}

	resolve := func(store *eventRetentionStore, id reservationID, commit bool) int64 {
		if commit {
			return store.commit(id, evt, size)
		}
		return store.rollback(id)
	}

	cases := []struct {
		name      string
		aCommits  bool
		bCommits  bool
		aFirst    bool
		wantBytes int64 // totalBytes once both A and B resolve
	}{
		{"A commits then B commits, A resolves first", true, true, true, size},
		{"A commits then B commits, B resolves first", true, true, false, size},
		{"A commits then B rolls back, A resolves first", true, false, true, 0},
		{"A commits then B rolls back, B resolves first", true, false, false, 0},
		{"A rolls back then B commits, A resolves first", false, true, true, size},
		{"A rolls back then B commits, B resolves first", false, true, false, size},
		{"A rolls back then B rolls back, A resolves first", false, false, true, 0},
		{"A rolls back then B rolls back, B resolves first", false, false, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newEventRetentionStore(1024)

			idA, _, ok := store.reserve(key, size)
			if !ok {
				t.Fatal("A's reserve failed")
			}
			// B's retry supersedes A's still-open reservation.
			idB, _, ok := store.reserve(key, size)
			if !ok {
				t.Fatal("B's reserve failed")
			}

			var metricSum int64
			if tc.aFirst {
				metricSum += resolve(store, idA, tc.aCommits)
				metricSum += resolve(store, idB, tc.bCommits)
			} else {
				metricSum += resolve(store, idB, tc.bCommits)
				metricSum += resolve(store, idA, tc.aCommits)
			}

			if ok, detail := store.checkInvariant(); !ok {
				t.Fatalf("invariant broken: %s", detail)
			}
			if n := store.pendingCount(); n != 0 {
				t.Fatalf("expected no reservation left open, got %d", n)
			}
			if store.totalBytes != tc.wantBytes {
				t.Errorf("expected totalBytes %d, got %d", tc.wantBytes, store.totalBytes)
			}
			if metricSum != tc.wantBytes {
				t.Errorf("expected the sum of every metric delta returned (%d) to equal totalBytes (%d): "+
					"a superseded reservation's commit or rollback must never add a second charge",
					metricSum, tc.wantBytes)
			}
		})
	}
}

// currentOpenReservationAcked reports whether the reservation the store
// currently has open for key both exists and carries acked, so a test can
// check the acked flag without reaching past the store's exported surface
// via anything but its own unexported fields (same package, test-only
// introspection).
func (s *eventRetentionStore) currentOpenReservationAcked(key retentionKey) (id reservationID, acked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, exists := s.openByKey[key]
	if !exists {
		return 0, false
	}
	p, exists := s.pending[id]
	return id, exists && p.acked
}

// hasRetained reports whether key currently holds a committed entry.
func (s *eventRetentionStore) hasRetained(key retentionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byKey[key]
	return ok
}

// TestEventRetentionInvariantUnderRandomizedInterleaving runs several
// hundred randomized reserve/commit/rollback/release calls across a
// handful of keys and asserts the accounting invariant after every single
// call. A key can carry more than one open reservation at once: a second
// reserve for a key supersedes the first inside the store without removing
// it from this driver's own bookkeeping, so a later commit or rollback
// against the first id exercises the stale-reservation path exactly as an
// overlapping retry would. The test also tracks the running sum of every
// metric delta commit and rollback return, alongside the store's own
// totalBytes, and checks the two match once every reservation resolves.
//
// release also runs against keys that carry an open reservation. Before
// every commit, the test checks whether the reservation commit is about to
// resolve was, at that moment, the one the store still has open for the
// key and carried an acked event_ack. When it was, the test asserts the
// key holds no retained entry once commit returns: a key acked after its
// last reservation opened and before that reservation's own commit runs
// must never end up retained.
//
// It is a sequential state-machine fuzz test, not a goroutine race test: it
// explores the interleavings a real concurrent scenario can produce without
// depending on goroutine timing.
func TestEventRetentionInvariantUnderRandomizedInterleaving(t *testing.T) {
	store := newEventRetentionStore(1 << 20)  // cap large enough that reserve essentially never refuses
	rng := rand.New(rand.NewSource(20260910)) // #nosec G404 -- test fixture, not crypto

	keys := make([]retentionKey, 6)
	for i := range keys {
		keys[i] = retentionKey{runID: fmt.Sprintf("r%d", i%3), seq: int64(i)}
	}

	// open tracks every reservation id the driver has issued for a key and
	// not yet resolved with a commit or rollback call, together with the
	// size that reserve call used.
	type openReservation struct {
		id   reservationID
		size int64
	}
	open := make(map[retentionKey][]openReservation)
	var metricSum int64
	var nextFakeID reservationID = 1 << 32 // an id the store itself never issues

	// takeOpen removes and returns a random one of key's still-unresolved
	// ids, or a never-issued id when none is open, so commit and rollback
	// are exercised with and without a prior reserve for this key.
	takeOpen := func(key retentionKey) (reservationID, int64) {
		list := open[key]
		if len(list) == 0 {
			nextFakeID++
			return nextFakeID, 1
		}
		j := rng.Intn(len(list))
		r := list[j]
		open[key] = append(list[:j], list[j+1:]...)
		return r.id, r.size
	}

	const iterations = 500
	for i := 0; i < iterations; i++ {
		key := keys[rng.Intn(len(keys))]

		switch rng.Intn(4) {
		case 0: // reserve
			size := int64(1 + rng.Intn(64))
			id, _, ok := store.reserve(key, size)
			if ok {
				open[key] = append(open[key], openReservation{id: id, size: size})
			}
		case 1: // commit
			id, size := takeOpen(key)
			openID, wasAcked := store.currentOpenReservationAcked(key)
			resolvesCurrentOpen := wasAcked && openID == id
			evt := &mitev1.BuildEvent{RunId: key.runID, Seq: key.seq, Phase: "STAGE", JobPath: "j", BuildNumber: 1}
			metricSum += store.commit(id, evt, size)
			if resolvesCurrentOpen && store.hasRetained(key) {
				t.Fatalf("iteration %d, key=%v: key was acked before this commit resolved its last-opened "+
					"reservation, but the key is retained after commit", i, key)
			}
		case 2: // rollback
			id, _ := takeOpen(key)
			metricSum += store.rollback(id)
		case 3: // release
			if freed, ok := store.release(key.runID, key.seq, ackStatusStored); ok {
				metricSum -= freed
			}
		}

		if ok, detail := store.checkInvariant(); !ok {
			t.Fatalf("invariant broken at iteration %d, key=%v: %s", i, key, detail)
		}
		if store.totalBytes < 0 {
			t.Fatalf("totalBytes went negative at iteration %d: %d", i, store.totalBytes)
		}
	}

	// Resolve every reservation the random sequence may have left open, so
	// a leaked reservation shows up as a nonzero pendingCount or a mismatch
	// between totalBytes and the sum over byKey.
	for _, list := range open {
		for _, r := range list {
			metricSum += store.rollback(r.id)
		}
	}
	if n := store.pendingCount(); n != 0 {
		t.Errorf("expected every reservation resolved, %d left open", n)
	}
	if ok, detail := store.checkInvariant(); !ok {
		t.Fatalf("invariant broken after draining pending reservations: %s", detail)
	}
	var sumKey int64
	for _, e := range store.byKey {
		sumKey += e.bytes
	}
	if store.totalBytes != sumKey {
		t.Errorf("expected totalBytes %d to equal the sum over byKey once every reservation is resolved, got %d",
			sumKey, store.totalBytes)
	}
	if store.totalBytes != metricSum {
		t.Errorf("expected the sum of every metric delta commit and rollback returned (%d) to equal totalBytes (%d)",
			metricSum, store.totalBytes)
	}
}

// TestEventsRetryOfExistingKeyAtCapSucceeds covers the review finding: at
// capacity, a retry of an event already retained must not be refused by
// reserve running before the duplicate replacement. The retry answers 202
// and totalBytes stays at the cap rather than growing or bouncing to 503.
func TestEventsRetryOfExistingKeyAtCapSucceeds(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"

	seq0 := newBuildEventJSON(0, "STARTED")
	size := decodedEventSize(t, seq0)
	agent.eventRetention = newEventRetentionStore(size) // cap == exactly one event
	var sendCount int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sendCount, 1)
		return nil
	}})

	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := agent.eventRetention.totalBytes; got != size {
		t.Fatalf("expected totalBytes %d after the first delivery, got %d", size, got)
	}

	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("retry at the cap: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := agent.eventRetention.totalBytes; got != size {
		t.Errorf("expected totalBytes unchanged at %d after the retry, got %d", size, got)
	}
	if atomic.LoadInt32(&sendCount) != 2 {
		t.Errorf("expected both deliveries forwarded on the wire, got %d sends", sendCount)
	}
}

// TestEventsLargerReplacementAtCapRefusedKeepsOldEntry covers the other
// side: a replacement that actually grows the retained size still cannot
// exceed the cap, refuses with 503, and leaves the earlier entry retained
// exactly as it was.
func TestEventsLargerReplacementAtCapRefusedKeepsOldEntry(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"

	small := newBuildEventJSON(0, "STARTED")
	smallSize := decodedEventSize(t, small)
	agent.eventRetention = newEventRetentionStore(smallSize) // cap == exactly the small event
	var sent []string
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = append(sent, m.GetBuildEvent().Phase)
		return nil
	}})

	if rec := postEvents(t, agent, small); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	// Same (run_id, event_seq), but a longer phase string so the replacement
	// encodes strictly larger than the original and cannot fit under the cap.
	larger := newBuildEventJSON(0, "STARTED_WITH_A_MUCH_LONGER_PHASE_STRING_THAN_BEFORE")
	if decodedEventSize(t, larger) <= smallSize {
		t.Fatal("test fixture: expected the replacement event to encode larger than the original")
	}
	rec := postEvents(t, agent, larger)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a larger replacement at the cap, got %d: %s", rec.Code, rec.Body.String())
	}

	got := agent.eventRetention.snapshotOrdered()
	if len(got) != 1 || got[0].Phase != "STARTED" {
		t.Fatalf("expected the original entry intact after the refused replacement, got %v", got)
	}
	if len(sent) != 1 || sent[0] != "STARTED" {
		t.Errorf("expected only the original event forwarded on the wire, got %v", sent)
	}
}

// TestEventsMidBatchFailureRetainsEarlierEventOnce covers the mid-batch
// failure scenario: seq 0 sends, seq 1 fails, so /v1/events answers 503
// and only the successful earlier event stays retained, once.
func TestEventsMidBatchFailureRetainsEarlierEventOnce(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	var sent []int64
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		evt := m.GetBuildEvent()
		if evt.Seq == 1 {
			return fmt.Errorf("boom")
		}
		sent = append(sent, evt.Seq)
		return nil
	}})
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	setTestConn(agent, conn)

	seq0 := newBuildEventJSON(0, "STARTED")
	seq1 := newBuildEventJSON(1, "STAGE")
	rec := postEvents(t, agent, seq0, seq1)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a mid-batch send failure, got %d", rec.Code)
	}
	if len(sent) != 1 || sent[0] != 0 {
		t.Fatalf("expected only seq 0 forwarded before the failure, got %v", sent)
	}

	got := agent.eventRetention.snapshotOrdered()
	if len(got) != 1 || got[0].Seq != 0 {
		t.Fatalf("expected only seq 0 retained after the mid-batch failure, got %v", got)
	}
	wantBytes := decodedEventSize(t, seq0)
	if agent.eventRetention.totalBytes != wantBytes {
		t.Errorf("expected totalBytes %d (seq 0 only), got %d", wantBytes, agent.eventRetention.totalBytes)
	}
}

// TestReplayAfterPartialBatchDeliverySendsEachRetainedEventOnce continues the
// mid-batch-failure scenario: the plugin retries the whole batch (seq 0
// duplicate, seq 1 now succeeds), then a reconnect replay must send each
// retained event exactly once, in sequence order.
func TestReplayAfterPartialBatchDeliverySendsEachRetainedEventOnce(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"

	failSeq1 := true
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		if m.GetBuildEvent().Seq == 1 && failSeq1 {
			return fmt.Errorf("boom")
		}
		return nil
	}})
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	setTestConn(agent, conn)

	seq0 := newBuildEventJSON(0, "STARTED")
	seq1 := newBuildEventJSON(1, "STAGE")
	if rec := postEvents(t, agent, seq0, seq1); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected the first delivery to 503 on seq 1, got %d", rec.Code)
	}

	// The plugin retries the whole batch once the mite reconnects and seq 1
	// can now send.
	failSeq1 = false
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})
	if rec := postEvents(t, agent, seq0, seq1); rec.Code != http.StatusAccepted {
		t.Fatalf("expected the retried delivery to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	var replayed []int64
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		replayed = append(replayed, m.GetBuildEvent().Seq)
		return nil
	}})
	agent.replayRetainedEvents()

	want := []int64{0, 1}
	if len(replayed) != len(want) {
		t.Fatalf("expected each retained event replayed once, got %v", replayed)
	}
	for i, w := range want {
		if replayed[i] != w {
			t.Errorf("replay order[%d] = %d, want %d (full: %v)", i, replayed[i], w, replayed)
		}
	}
}

// TestReplayAfterDuplicateDeliverySendsSeqOnce covers the review scenario's
// closing check: after a duplicate delivery of the same event, a replay
// sends it once, not twice.
func TestReplayAfterDuplicateDeliverySendsSeqOnce(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	seq0 := newBuildEventJSON(0, "STARTED")
	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: expected 202, got %d", rec.Code)
	}
	if rec := postEvents(t, agent, seq0); rec.Code != http.StatusAccepted {
		t.Fatalf("retried delivery: expected 202, got %d", rec.Code)
	}

	var replayed []int64
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		replayed = append(replayed, m.GetBuildEvent().Seq)
		return nil
	}})
	agent.replayRetainedEvents()

	if len(replayed) != 1 || replayed[0] != 0 {
		t.Fatalf("expected seq 0 replayed exactly once, got %v", replayed)
	}
}

// --- 4.4: claim route ---

func claimRequestBody() string {
	return `{"origin":"scm","intent_key":"k","run_id":"r","job_path":"j"}`
}

// oversizedClaimBody is a syntactically valid ClaimRequest object well past
// 1 MiB: encoding/json ignores the unknown "_pad" field, but the decoder
// must still read through its string value, exercising the MaxBytesReader
// cap rather than failing fast on a syntax error.
func oversizedClaimBody() string {
	return fmt.Sprintf(`{"origin":"scm","intent_key":"k","run_id":"r","job_path":"j","_pad":%q}`,
		strings.Repeat("a", loopbackMaxBodyBytes+1))
}

func TestClaimTwoInFlightDoNotCross(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 2 * time.Second

	captured := make(chan string, 2)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		captured <- m.GetClaimRequest().RequestId
		return nil
	}})

	results := make(chan *httptest.ResponseRecorder, 2)
	post := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		agent.handleClaim(rec, req)
		results <- rec
	}
	go post()
	go post()

	id1 := <-captured
	id2 := <-captured
	if id1 == id2 {
		t.Fatalf("expected distinct request ids, got the same value twice: %s", id1)
	}

	// Resolve the second claim first; only its caller may be released.
	agent.claimWaiters.resolve(&mitev1.ClaimResponse{RequestId: id2, Decision: "granted", RunId: "r", BuildNumber: 2})

	var first *mitev1.ClaimResponse
	select {
	case rec := <-results:
		var resp mitev1.ClaimResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		first = &resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the released claim")
	}
	if first.RequestId != id2 {
		t.Errorf("expected the second claim's response to be released first, got request id %s", first.RequestId)
	}

	// Release the first waiter so its goroutine does not leak past the test.
	agent.claimWaiters.resolve(&mitev1.ClaimResponse{RequestId: id1, Decision: "granted", RunId: "r", BuildNumber: 1})
	select {
	case <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first claim to finish")
	}
}

func TestClaim504Timeout(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 30 * time.Millisecond
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleClaim(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp mitev1.ClaimResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Decision != "" || resp.Code != unavailableCode {
		t.Errorf("expected empty decision / %s code, got %+v", unavailableCode, resp)
	}
	assertUnavailableWireShape(t, rec.Body.Bytes())
}

func TestClaim429Cap(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	for i := 0; i < maxClaimWaiters; i++ {
		if _, ok := agent.claimWaiters.register(fmt.Sprintf("filler-%d", i)); !ok {
			t.Fatalf("failed to fill the waiter map at entry %d", i)
		}
	}
	var sent int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleClaim(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 above the waiter cap, got %d", rec.Code)
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Errorf("expected no send above the cap, got %d", sent)
	}
}

func TestClaimFailureBodyIsOneShape(t *testing.T) {
	cases := []struct {
		name       string
		wantStatus int
		setup      func() *http.Request
	}{
		{
			name:       "wrong bearer",
			wantStatus: http.StatusUnauthorized,
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
				req.Header.Set("Authorization", "Bearer wrong")
				return req
			},
		},
		{
			name:       "malformed body",
			wantStatus: http.StatusBadRequest,
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader("not-json"))
				req.Header.Set("Authorization", "Bearer tok")
				return req
			},
		},
		{
			name:       "oversized body",
			wantStatus: http.StatusRequestEntityTooLarge,
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(oversizedClaimBody()))
				req.Header.Set("Authorization", "Bearer tok")
				return req
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := mustNewAgent(t, Config{})
			agent.Logger = slog.Default()
			agent.loopbackToken = "tok"
			rec := httptest.NewRecorder()
			agent.handleClaim(rec, tc.setup())
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			var resp mitev1.ClaimResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
			}
			if resp.Decision != "" || resp.Code != unavailableCode {
				t.Errorf("expected empty decision / %s code, got %+v", unavailableCode, resp)
			}
			assertUnavailableWireShape(t, rec.Body.Bytes())
		})
	}
}

// assertUnavailableWireShape pins the raw failure body the plugin reads.
// The body holds the failure code and no decision key, because the
// response type encodes decision with omitempty. A decode into the
// struct cannot see the difference, so this reads the raw keys.
func assertUnavailableWireShape(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw body: %v (%s)", err, body)
	}
	if raw["code"] != unavailableCode {
		t.Errorf("raw body code = %v, want %q: %s", raw["code"], unavailableCode, body)
	}
	if _, ok := raw["decision"]; ok {
		t.Errorf("raw body holds a decision key, want none: %s", body)
	}
}

func TestClaimRelaysAGatewayUnavailableReplyWithNoDecisionKey(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 2 * time.Second

	captured := make(chan string, 1)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		captured <- m.GetClaimRequest().RequestId
		return nil
	}})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		agent.handleClaim(rec, req)
		done <- rec
	}()

	id := <-captured
	agent.claimWaiters.resolve(&mitev1.ClaimResponse{
		RequestId: id,
		Code:      unavailableCode,
		Error:     "ledger claim request failed: nats: no responders available for request",
	})

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		assertUnavailableWireShape(t, rec.Body.Bytes())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the relayed claim reply")
	}
}

// --- 4.4a: cell_instance ---

type fakeMiteClient struct {
	mu          sync.Mutex
	registerReq *mitev1.RegisterRequest
}

func (f *fakeMiteClient) Register(_ context.Context, req *mitev1.RegisterRequest, _ ...grpc.CallOption) (*mitev1.RegisterResponse, error) {
	f.mu.Lock()
	f.registerReq = req
	f.mu.Unlock()
	return nil, fmt.Errorf("fakeMiteClient: register stub always fails")
}

func (f *fakeMiteClient) CommandStream(_ context.Context, _ ...grpc.CallOption) (mitev1.Mite_CommandStreamClient, error) {
	return nil, fmt.Errorf("fakeMiteClient: command stream not implemented")
}

func TestRegisterCarriesCellInstance(t *testing.T) {
	fc := &fakeMiteClient{}
	agent := mustNewAgent(t, Config{ControllerName: "c", Namespace: "ns", CellInstance: "pod-uid-123"})
	agent.Logger = slog.Default()
	agent.client = fc

	id := &identityState{isBootstrap: true, bootstrapToken: "tok"}
	_ = agent.register(context.Background(), id) // the stub error is expected; only the request matters

	if fc.registerReq == nil {
		t.Fatal("Register was never called")
	}
	if fc.registerReq.CellInstance != "pod-uid-123" {
		t.Errorf("expected RegisterRequest.cell_instance %q, got %q", "pod-uid-123", fc.registerReq.CellInstance)
	}
}

func TestRenewCarriesCellInstance(t *testing.T) {
	fc := &fakeMiteClient{}
	agent := mustNewAgent(t, Config{ControllerName: "c", Namespace: "ns", CellInstance: "pod-uid-456"})
	agent.Logger = slog.Default()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	agent.keypair = priv

	_ = agent.renewWithExistingKeypair(context.Background(), fc)

	if fc.registerReq == nil || fc.registerReq.CellInstance != "pod-uid-456" {
		t.Errorf("expected renewal RegisterRequest.cell_instance %q, got %+v", "pod-uid-456", fc.registerReq)
	}
}

func TestClaimCellInstanceOverwritesBody(t *testing.T) {
	agent := mustNewAgent(t, Config{CellInstance: "pod-uid-789"})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 50 * time.Millisecond // no response injected: fail fast via 504

	var captured *mitev1.ClaimRequest
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		captured = m.GetClaimRequest()
		return nil
	}})

	body := `{"origin":"scm","intent_key":"k","run_id":"r","job_path":"j","cell_instance":"attacker-supplied"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleClaim(rec, req)

	if captured == nil {
		t.Fatal("claim_request was not sent")
	}
	if captured.CellInstance != "pod-uid-789" {
		t.Errorf("expected cell_instance overwritten to %q, got %q", "pod-uid-789", captured.CellInstance)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected 504 (no response injected), got %d", rec.Code)
	}
}

// TestClaimPassesStartMark proves the claim route forwards a posted
// start_mark unchanged, so the plugin's start mark reaches the gateway on
// the claim's own stream message.
func TestClaimPassesStartMark(t *testing.T) {
	agent := mustNewAgent(t, Config{CellInstance: "pod-uid-789"})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 50 * time.Millisecond

	var captured *mitev1.ClaimRequest
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		captured = m.GetClaimRequest()
		return nil
	}})

	body := `{"origin":"scm","intent_key":"k","run_id":"r","job_path":"j","start_mark":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	agent.handleClaim(httptest.NewRecorder(), req)

	if captured == nil {
		t.Fatal("claim_request was not sent")
	}
	if !captured.StartMark {
		t.Error("expected start_mark forwarded as true")
	}
	if captured.CellInstance != "pod-uid-789" {
		t.Errorf("expected cell_instance %q on the start mark, got %q", "pod-uid-789", captured.CellInstance)
	}
}

// --- 4.5: processCommands cases + teardown release ---

func TestProcessCommandsResolvesClaimAndReleasesEventAck(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()

	evt := &mitev1.BuildEvent{RunId: "r1", Seq: 9, Phase: "COMPLETED", JobPath: "j", BuildNumber: 1}
	size := eventSize(evt)
	id, _, _ := agent.eventRetention.reserve(retentionKey{runID: evt.RunId, seq: evt.Seq}, size)
	agent.eventRetention.commit(id, evt, size)

	ch, ok := agent.claimWaiters.register("req-42")
	if !ok {
		t.Fatal("register failed")
	}

	msgs := []*mitev1.OperatorMessage{
		{Message: &mitev1.OperatorMessage_ClaimResponse{ClaimResponse: &mitev1.ClaimResponse{
			RequestId: "req-42", Decision: "granted", RunId: "r1", BuildNumber: 3,
		}}},
		{Message: &mitev1.OperatorMessage_EventAck{EventAck: &mitev1.EventAck{
			RunId: "r1", EventSeq: 9, Status: ackStatusStored,
		}}},
	}
	idx := 0
	setTestStream(agent, &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error { return nil },
		recvFn: func() (*mitev1.OperatorMessage, error) {
			if idx < len(msgs) {
				m := msgs[idx]
				idx++
				return m, nil
			}
			return nil, io.EOF
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		agent.processCommands(ctx)
		close(done)
	}()

	select {
	case resp := <-ch:
		if resp.Decision != "granted" {
			t.Errorf("expected granted decision, got %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the claim response")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processCommands did not return after the stream ended")
	}

	if got := agent.eventRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected the acked event released, still retained: %v", got)
	}
	if acks := agent.eventAcks.drain(); len(acks) != 1 || acks[0].RunId != "r1" {
		t.Errorf("expected the ack buffered for the plugin, got %+v", acks)
	}
}

// TestProcessCommandsTeardownReleasesClaimWaiters is 4.5's acceptance test:
// no goroutine remains blocked on a claim after a teardown with two waiting
// claims. It checks this with a done-channel per waiter rather than goleak,
// which is not a repo dependency.
func TestProcessCommandsTeardownReleasesClaimWaiters(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()

	recvBlock := make(chan struct{})
	setTestStream(agent, &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error { return nil },
		recvFn: func() (*mitev1.OperatorMessage, error) {
			<-recvBlock
			return nil, io.EOF
		},
	})

	ch1, ok1 := agent.claimWaiters.register("req-1")
	ch2, ok2 := agent.claimWaiters.register("req-2")
	if !ok1 || !ok2 {
		t.Fatal("failed to register the two waiters")
	}

	released := make(chan string, 2)
	waitFor := func(id string, ch chan *mitev1.ClaimResponse) {
		<-ch // blocks until releaseAll (or a real resolve) delivers a value
		released <- id
	}
	go waitFor("req-1", ch1)
	go waitFor("req-2", ch2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		agent.processCommands(ctx)
		close(done)
	}()

	close(recvBlock) // let Recv return io.EOF: stream teardown

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processCommands did not return after stream teardown")
	}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-released:
			got[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for waiter release (%d of 2 released)", i)
		}
	}
	if !got["req-1"] || !got["req-2"] {
		t.Errorf("expected both waiters released on teardown, got %v", got)
	}
}

// TestClaimRelaysTheHomeSlotOfANotHomeVeto asserts the /v1/claim body
// carries home_slot when the gateway sets it, slot 0 included, and omits
// the key when the gateway sets none.
func TestClaimRelaysTheHomeSlotOfANotHomeVeto(t *testing.T) {
	zero := 0
	cases := []struct {
		name     string
		homeSlot *int
		want     string
	}{
		{"slot 0 is relayed", &zero, `"home_slot":0`},
		{"no slot is omitted", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := mustNewAgent(t, Config{})
			agent.Logger = slog.Default()
			agent.loopbackToken = "tok"
			agent.claimTimeout = 2 * time.Second
			captured := make(chan string, 1)
			setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
				captured <- m.GetClaimRequest().RequestId
				return nil
			}})

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
				req.Header.Set("Authorization", "Bearer tok")
				rec := httptest.NewRecorder()
				agent.handleClaim(rec, req)
				done <- rec
			}()
			id := <-captured
			agent.claimWaiters.resolve(&mitev1.ClaimResponse{RequestId: id, Decision: "vetoed",
				ReasonCode: "NOT_HOME", HomeSlot: tc.homeSlot})

			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the claim reply")
			}
			body := rec.Body.String()
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Fatalf("body %s lacks %s", body, tc.want)
			}
			if tc.want == "" && strings.Contains(body, "home_slot") {
				t.Fatalf("body %s carries home_slot, want none", body)
			}
		})
	}
}
