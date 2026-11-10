package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// fakeClock is a settable clock for the replay ledger tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestReplayLedger_AcceptedAfterRecord(t *testing.T) {
	l := newReplayLedger((&fakeClock{t: time.Unix(1000, 0)}).now)
	if _, ok := l.accepted("k"); ok {
		t.Fatal("unknown key reported accepted")
	}
	l.record("k", 200)
	status, ok := l.accepted("k")
	if !ok || status != 200 {
		t.Fatalf("accepted = (%d, %v), want (200, true)", status, ok)
	}
}

func TestReplayLedger_ExpiresAfterTTL(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	l := newReplayLedger(clock.now)
	l.record("k", 200)
	clock.t = clock.t.Add(replayLedgerTTL - time.Second)
	if _, ok := l.accepted("k"); !ok {
		t.Fatal("entry expired before the TTL")
	}
	clock.t = clock.t.Add(time.Second)
	if _, ok := l.accepted("k"); ok {
		t.Fatal("entry still accepted at the TTL")
	}
}

func TestReplayLedger_EvictsOldestOverCap(t *testing.T) {
	l := newReplayLedger((&fakeClock{t: time.Unix(1000, 0)}).now)
	for i := 0; i <= replayLedgerCap; i++ {
		l.record(replayTestKey(i), 200)
	}
	if len(l.entries) != replayLedgerCap || len(l.order) != replayLedgerCap {
		t.Fatalf("size entries=%d order=%d, want %d", len(l.entries), len(l.order), replayLedgerCap)
	}
	if _, ok := l.accepted(replayTestKey(0)); ok {
		t.Fatal("oldest entry survived the cap")
	}
	if _, ok := l.accepted(replayTestKey(replayLedgerCap)); !ok {
		t.Fatal("newest entry was evicted")
	}
}

func replayTestKey(i int) string {
	return "key-" + time.Duration(i).String()
}

func TestReplayLedger_NilAndEmptyKeyAreInert(t *testing.T) {
	var nilLedger *replayLedger
	nilLedger.record("k", 200)
	if _, ok := nilLedger.accepted("k"); ok {
		t.Fatal("nil ledger reported accepted")
	}
	l := newReplayLedger(time.Now)
	l.record("", 200)
	if len(l.entries) != 0 {
		t.Fatal("empty key was recorded")
	}
}

func TestReplayKey_EmptyForMissingOrZeroCommandID(t *testing.T) {
	if got := replayKey(nil); got != "" {
		t.Errorf("nil command key = %q, want empty", got)
	}
	for _, id := range []string{"", "0"} {
		cmd := &mitev1.ImperativeCommand{CommandId: id, ReplayWebhook: &mitev1.ReplayWebhookPayload{DeliveryId: "d"}}
		if got := replayKey(cmd); got != "" {
			t.Errorf("CommandId %q key = %q, want empty", id, got)
		}
	}
}

func TestReplayKey_DiffersByDeliveryAndBody(t *testing.T) {
	base := func(delivery, body string) *mitev1.ImperativeCommand {
		return &mitev1.ImperativeCommand{
			CommandId:     "42",
			ReplayWebhook: &mitev1.ReplayWebhookPayload{DeliveryId: delivery, Body: []byte(body)},
		}
	}
	same := replayKey(base("d1", "b"))
	if same == "" {
		t.Fatal("key is empty for a command with an identifier")
	}
	if replayKey(base("d1", "b")) != same {
		t.Error("identical replays produced different keys")
	}
	if replayKey(base("d2", "b")) == same {
		t.Error("different delivery produced the same key")
	}
	if replayKey(base("d1", "c")) == same {
		t.Error("different body produced the same key")
	}
}

// startReplayUpstream serves localhost:8080 for a replay test. It counts
// each POST and answers status(n), where n is the 1-based POST count. It
// skips the test when the port is busy.
func startReplayUpstream(t *testing.T, status func(n int32) int) *atomic.Int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skip("skipping: localhost:8080 not available:", err)
	}
	var posts atomic.Int32
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := posts.Add(1)
			w.WriteHeader(status(n))
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return &posts
}

// replayRepeatCmd returns one webhook replay as the operator resends it.
// The command identifier and the delivery identifier stay the same.
func replayRepeatCmd() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "replay-repeat-001",
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:       "github-webhook/",
			Body:       []byte("payload-body"),
			DeliveryId: "delivery-repeat",
		},
	}
}

// newReplayRepeatAgent returns an agent whose replays reach the upstream
// that startReplayUpstream serves, and the stream that captures results.
func newReplayRepeatAgent(t *testing.T) (*Agent, func() *mitev1.CommandResult) {
	t.Helper()
	stream, captured := testReplayStream(t)
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: "http://localhost:1"})
	agent.Logger = slog.Default()
	setTestStream(agent, stream)
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	return agent, captured.Load
}

// TestReplayWebhookRepeatAnswersEarlierResultWithoutPost verifies that a
// resend of an accepted replay answers the earlier 2xx and posts nothing.
func TestReplayWebhookRepeatAnswersEarlierResultWithoutPost(t *testing.T) {
	posts := startReplayUpstream(t, func(int32) int { return http.StatusOK })
	agent, result := newReplayRepeatAgent(t)

	for i := 1; i <= 2; i++ {
		agent.runCommand(context.Background(), commandWork{imperativeCmd: replayRepeatCmd(), ctx: context.Background()})
		if r := result(); r == nil || !r.Success || r.HttpStatus != http.StatusOK {
			t.Fatalf("attempt %d: want success with status 200, got %+v", i, r)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("upstream POSTs = %d, want 1", got)
	}
}

// TestReplayWebhookRepeatPostsAgainAfterNon2xx verifies that a replay which
// Jenkins refused is not remembered, so the operator's resend posts again.
func TestReplayWebhookRepeatPostsAgainAfterNon2xx(t *testing.T) {
	posts := startReplayUpstream(t, func(n int32) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	agent, result := newReplayRepeatAgent(t)

	agent.runCommand(context.Background(), commandWork{imperativeCmd: replayRepeatCmd(), ctx: context.Background()})
	if r := result(); r == nil || r.Success {
		t.Fatalf("first attempt: want failure for 503, got %+v", r)
	}
	agent.runCommand(context.Background(), commandWork{imperativeCmd: replayRepeatCmd(), ctx: context.Background()})
	if r := result(); r == nil || !r.Success {
		t.Fatalf("second attempt: want success, got %+v", r)
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("upstream POSTs = %d, want 2", got)
	}
}
