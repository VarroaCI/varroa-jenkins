package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// newLogRequest builds a POST /v1/log request carrying the nine
// X-Varroa-* headers proposal.md names, plus the bearer and body. Every
// caller in this file names the same run, "run-1", and none needs the
// final chunk (X-Varroa-Final is always false).
func newLogRequest(token, jobPath string, buildNumber, seq int64, nodeID, stageID string, firstLineMS, dropped int64, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(hdrLogRunID, "run-1")
	req.Header.Set(hdrLogJobPath, url.QueryEscape(jobPath))
	req.Header.Set(hdrLogBuildNumber, strconv.FormatInt(buildNumber, 10))
	req.Header.Set(hdrLogSeq, strconv.FormatInt(seq, 10))
	req.Header.Set(hdrLogNodeID, nodeID)
	req.Header.Set(hdrLogStageID, stageID)
	req.Header.Set(hdrLogFirstLineUnixMs, strconv.FormatInt(firstLineMS, 10))
	req.Header.Set(hdrLogDroppedBytes, strconv.FormatInt(dropped, 10))
	req.Header.Set(hdrLogFinal, strconv.FormatBool(false))
	return req
}

// --- 2.1: header mapping ---

func TestLogHeaderMappingIncludesStampedInstance(t *testing.T) {
	agent := NewAgent(Config{CellInstance: "pod-uid-1"})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	var sent *mitev1.LogChunk
	agent.stream = &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = m.GetLogChunk()
		return nil
	}}

	req := newLogRequest("tok", "folder/job", 42, 7, "17", "12", 1757000000123, 5, "hello console")
	rec := httptest.NewRecorder()
	agent.handleLog(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if sent == nil {
		t.Fatal("expected a LogChunk forwarded on the stream")
	}
	want := &mitev1.LogChunk{
		RunId: "run-1", JobPath: "folder/job", BuildNumber: 42, Seq: 7,
		NodeId: "17", StageId: "12", FirstLineUnixMs: 1757000000123, DroppedBytes: 5,
		Final: false, CellInstance: "pod-uid-1", Data: []byte("hello console"),
	}
	if sent.RunId != want.RunId || sent.JobPath != want.JobPath || sent.BuildNumber != want.BuildNumber ||
		sent.Seq != want.Seq || sent.NodeId != want.NodeId || sent.StageId != want.StageId ||
		sent.FirstLineUnixMs != want.FirstLineUnixMs || sent.DroppedBytes != want.DroppedBytes ||
		sent.Final != want.Final || sent.CellInstance != want.CellInstance || string(sent.Data) != string(want.Data) {
		t.Fatalf("mapped chunk mismatch: got %+v (data=%q), want %+v (data=%q)", sent, sent.Data, want, want.Data)
	}
}

// --- 2.2: response contract table ---

func TestLogResponseContractTable(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(a *Agent)
		token      string
		malformed  bool
		wantStatus int
		wantMaxHdr bool
	}{
		{
			name:       "success",
			setup:      func(a *Agent) {},
			token:      "tok",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "malformed metadata",
			setup:      func(a *Agent) {},
			token:      "tok",
			malformed:  true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "bad bearer",
			setup:      func(a *Agent) {},
			token:      "wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no stream open",
			setup:      func(a *Agent) { a.stream = nil },
			token:      "tok",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "retained buffer at bound",
			setup:      func(a *Agent) { a.logRetention = newLogRetentionStore(1) },
			token:      "tok",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "busy pause window open",
			setup: func(a *Agent) {
				a.logRetention.pause("run-1", time.Minute)
			},
			token:      "tok",
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := NewAgent(Config{})
			agent.Logger = slog.Default()
			agent.loopbackToken = "tok"
			agent.stream = &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }}
			tc.setup(agent)

			var req *http.Request
			if tc.malformed {
				req = httptest.NewRequest(http.MethodPost, "/v1/log", strings.NewReader("x"))
				req.Header.Set("Authorization", "Bearer "+tc.token)
				// run_id missing entirely -> malformed.
			} else {
				req = newLogRequest(tc.token, "j", 1, 1, "", "", 0, 0, "xx")
			}
			rec := httptest.NewRecorder()
			agent.handleLog(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestLogOversizedBody413CarriesMaxChunkHeader(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.stream = &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }}

	big := strings.Repeat("x", logChunkMaxBytes+1)
	req := newLogRequest("tok", "j", 1, 1, "", "", 0, 0, big)
	rec := httptest.NewRecorder()
	agent.handleLog(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
	if rec.Header().Get(hdrLogMaxChunkBytes) != strconv.Itoa(logChunkMaxBytes) {
		t.Errorf("expected %s header = %d, got %q", hdrLogMaxChunkBytes, logChunkMaxBytes, rec.Header().Get(hdrLogMaxChunkBytes))
	}
}

// --- 2.5: retained buffer bound and hold-until-ack ---

func TestLogRetentionBoundAnswers503AndDropsNothingRetained(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.logRetention = newLogRetentionStore(10) // tiny cap
	var sent int32
	agent.stream = &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}}

	req := newLogRequest("tok", "j", 1, 1, "", "", 0, 0, "this is far more than ten bytes")
	rec := httptest.NewRecorder()
	agent.handleLog(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 at the retention bound, got %d", rec.Code)
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Errorf("expected nothing forwarded at the cap, got %d sends", sent)
	}
	if got := agent.logRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected no retained chunk at refusal, got %v", got)
	}
}

func TestLogRetentionHoldsChunkUntilAck(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.stream = &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }}

	req := newLogRequest("tok", "j", 1, 3, "", "", 0, 0, "payload")
	rec := httptest.NewRecorder()
	agent.handleLog(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if got := agent.logRetention.snapshotOrdered(); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("expected the chunk retained until ack, got %v", got)
	}

	agent.handleLogAck(context.Background(), &mitev1.LogAck{RunId: "run-1", AckSeq: 3, Status: ackStatusStored})
	if got := agent.logRetention.snapshotOrdered(); len(got) != 0 {
		t.Errorf("expected release on STORED, still retained: %v", got)
	}
}

// --- 2.6: log_ack table, cumulative release ---

func TestLogAckTableCumulativeRelease(t *testing.T) {
	newRetained := func(a *Agent) {
		a.stream = &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }}
		for seq := int64(1); seq <= 3; seq++ {
			req := newLogRequest("tok", "j", 1, seq, "", "", 0, 0, "x")
			rec := httptest.NewRecorder()
			a.handleLog(rec, req)
			if rec.Code != http.StatusAccepted {
				panic(fmt.Sprintf("setup send seq %d failed: %d", seq, rec.Code))
			}
		}
	}

	t.Run("STORED releases at or below ack_seq", func(t *testing.T) {
		agent := NewAgent(Config{})
		agent.Logger = slog.Default()
		agent.loopbackToken = "tok"
		newRetained(agent)

		agent.handleLogAck(context.Background(), &mitev1.LogAck{RunId: "run-1", AckSeq: 2, Status: ackStatusStored})
		got := agent.logRetention.snapshotOrdered()
		if len(got) != 1 || got[0].Seq != 3 {
			t.Fatalf("expected only seq 3 retained after STORED ack_seq=2, got %v", got)
		}
	})

	t.Run("BUSY keeps every chunk and opens a pause window", func(t *testing.T) {
		agent := NewAgent(Config{})
		agent.Logger = slog.Default()
		agent.loopbackToken = "tok"
		newRetained(agent)

		agent.handleLogAck(context.Background(), &mitev1.LogAck{RunId: "run-1", Status: ackStatusBusy, PauseMs: 5000})
		if got := agent.logRetention.snapshotOrdered(); len(got) != 3 {
			t.Fatalf("expected every chunk retained on BUSY, got %v", got)
		}
		if !agent.logRetention.paused("run-1") {
			t.Error("expected the pause window open after BUSY")
		}
		req := newLogRequest("tok", "j", 1, 4, "", "", 0, 0, "x")
		rec := httptest.NewRecorder()
		agent.handleLog(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503 while the BUSY pause window is open, got %d", rec.Code)
		}
	})

	t.Run("REJECTED releases ack_seq and every earlier chunk", func(t *testing.T) {
		agent := NewAgent(Config{})
		agent.Logger = slog.Default()
		agent.loopbackToken = "tok"
		newRetained(agent)

		agent.handleLogAck(context.Background(), &mitev1.LogAck{RunId: "run-1", AckSeq: 2, Status: ackStatusRejected, Reason: "foreign-run"})
		got := agent.logRetention.snapshotOrdered()
		if len(got) != 1 || got[0].Seq != 3 {
			t.Fatalf("expected only seq 3 retained after REJECTED ack_seq=2, got %v", got)
		}
	})
}

// --- 2.7: replay on reconnect ---

func TestLogReplayAfterReconnectOrderedPerRun(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.logRetention = newLogRetentionStore(1 << 30)

	for _, seq := range []int64{3, 1, 2} {
		chunk := &mitev1.LogChunk{RunId: "run-1", Seq: seq, Data: []byte("x")}
		agent.logRetention.reserve(1)
		agent.logRetention.commit(chunk, 1)
	}

	var mu sync.Mutex
	var sentSeqs []int64
	agent.stream = &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		mu.Lock()
		sentSeqs = append(sentSeqs, m.GetLogChunk().Seq)
		mu.Unlock()
		return nil
	}}

	agent.replayRetainedLogChunks()

	mu.Lock()
	defer mu.Unlock()
	if len(sentSeqs) != 3 || sentSeqs[0] != 1 || sentSeqs[1] != 2 || sentSeqs[2] != 3 {
		t.Fatalf("expected replay in ascending seq order, got %v", sentSeqs)
	}
}

// --- 2.4: tail_control handling ---

// TestHandleTailControlCallsJenkinsAndAcks starts a stub Jenkins on
// localhost:8080 (skipped when the port is unavailable, matching the
// REPLAY_WEBHOOK tests' pattern), calls handleTailControl, and asserts
// the call it made and the TailAck it sent.
func TestHandleTailControlCallsJenkinsAndAcks(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skip("skipping: localhost:8080 not available:", err)
	}
	var gotPath, gotAuth string
	var gotBody tailControlRequest
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	time.Sleep(10 * time.Millisecond)

	var acked *mitev1.TailAck
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.stream = &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		acked = m.GetTailAck()
		return nil
	}}
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "jwt-token"
	agent.jenkinsTokenMu.Unlock()

	tc := &mitev1.TailControl{
		RunId: "run-1", JobPath: "folder/job", BuildNumber: 9,
		Action: "start", LeaseSeconds: 90, ControlId: "ctl-1",
	}
	agent.handleTailControl(context.Background(), tc)

	if gotPath != varroaLogTailPath {
		t.Errorf("expected call to %s, got %s", varroaLogTailPath, gotPath)
	}
	if gotAuth != "Bearer jwt-token" {
		t.Errorf("expected the cached Jenkins bearer JWT, got %q", gotAuth)
	}
	if gotBody.RunID != "run-1" || gotBody.Action != "start" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	if acked == nil {
		t.Fatal("expected a TailAck sent")
	}
	if acked.ControlId != "ctl-1" || acked.RunId != "run-1" || !acked.Active {
		t.Errorf("unexpected TailAck: %+v", acked)
	}
}

// --- 2.3: race between /v1/log posts and heartbeats ---

func TestLogSendRaceWithHeartbeats(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.heartbeatInterval = time.Millisecond
	var sendCount int32
	agent.stream = &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error {
			atomic.AddInt32(&sendCount, 1)
			return nil
		},
		recvFn: func() (*mitev1.OperatorMessage, error) {
			<-context.Background().Done()
			return nil, context.Canceled
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go agent.sendHeartbeats(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(0); i < 50; i++ {
			req := newLogRequest("tok", "j", 1, i, "", "", 0, 0, "x")
			rec := httptest.NewRecorder()
			agent.handleLog(rec, req)
		}
	}()
	<-done
	<-ctx.Done()

	if atomic.LoadInt32(&sendCount) == 0 {
		t.Fatal("expected at least one Send call from the heartbeat goroutine or the log posts")
	}
}
