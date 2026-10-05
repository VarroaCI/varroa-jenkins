package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// heldApprover holds every approve for the controller named "held" until
// release closes, as an approve that waits for the controller lock does.
// It answers every other approve at once.
type heldApprover struct {
	started chan struct{}
	release chan struct{}
}

func newHeldApprover() *heldApprover {
	return &heldApprover{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (h *heldApprover) hold(ctx context.Context, name string) error {
	if name != "held" {
		return nil
	}
	h.started <- struct{}{}
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *heldApprover) ApproveRestart(ctx context.Context, _, _, name, _, _ string) error {
	return h.hold(ctx, name)
}

func (h *heldApprover) ApproveDeletion(ctx context.Context, _, _, name, _ string) error {
	return h.hold(ctx, name)
}

// startApproveRunner runs start until the test ends and answers a client
// connection to the same NATS server.
func startApproveRunner(t *testing.T, start func(ctx context.Context, conn *bus.Conn) error) *bus.Conn {
	t.Helper()
	s := startLedgerActionNATS(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect operator: %v", err)
	}
	t.Cleanup(conn.Close)
	client, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect client: %v", err)
	}
	t.Cleanup(client.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = start(ctx, conn)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return client
}

// requestInBackground sends one request and answers its reply payload, or
// nil on an error, on the returned channel.
func requestInBackground(client *bus.Conn, subject string, data []byte) <-chan []byte {
	out := make(chan []byte, 1)
	go func() {
		msg, err := client.Request(subject, data, 10*time.Second)
		if err != nil {
			out <- nil
			return
		}
		out <- msg.Data
	}()
	return out
}

// waitSubscribed polls until a request for a free controller gets a reply,
// so the runner's subscription exists before the test sends its requests.
func waitSubscribed(t *testing.T, client *bus.Conn, subject string, data []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Request(subject, data, 200*time.Millisecond); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no approve runner answered on %s", subject)
}

func waitHeld(t *testing.T, h *heldApprover) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held approve did not reach the reconciler")
	}
}

// TestApproveRestartRunner_SecondControllerAnswersWhileFirstWaits pins
// that an approve restart that waits for one controller does not hold the
// approve restart of another controller on the same replica.
func TestApproveRestartRunner_SecondControllerAnswersWhileFirstWaits(t *testing.T) {
	h := newHeldApprover()
	subject := bus.OperatorApproveSubject("core")
	client := startApproveRunner(t, func(ctx context.Context, conn *bus.Conn) error {
		r := &approveRestartRunner{busConn: conn, cluster: "core", reconciler: h, logger: slog.Default()}
		return r.Start(ctx)
	})
	free := mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "free", Action: "approve"})
	waitSubscribed(t, client, subject, free)

	held := requestInBackground(client, subject,
		mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "held", Action: "approve"}))
	waitHeld(t, h)

	msg, err := client.Request(subject, free, time.Second)
	if err != nil {
		t.Fatalf("the approve for another controller waited behind the held one: %v", err)
	}
	var resp bus.ApproveRestartResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != "" {
		t.Fatalf("free reply = %s (decode error %v), want success", msg.Data, err)
	}

	close(h.release)
	if got := <-held; got == nil {
		t.Fatal("the held approve got no reply after its release")
	}
}

// TestApproveDeletionRunner_SecondControllerAnswersWhileFirstWaits pins
// the same rule for approve deletion.
func TestApproveDeletionRunner_SecondControllerAnswersWhileFirstWaits(t *testing.T) {
	h := newHeldApprover()
	subject := bus.OperatorApproveDeletionSubject("core")
	client := startApproveRunner(t, func(ctx context.Context, conn *bus.Conn) error {
		r := &approveDeletionRunner{busConn: conn, cluster: "core", reconciler: h, logger: slog.Default()}
		return r.Start(ctx)
	})
	free := mustJSON(t, bus.ApproveDeletionRequest{Namespace: "team-a", Name: "free", Path: "folder/job"})
	waitSubscribed(t, client, subject, free)

	held := requestInBackground(client, subject,
		mustJSON(t, bus.ApproveDeletionRequest{Namespace: "team-a", Name: "held", Path: "folder/job"}))
	waitHeld(t, h)

	msg, err := client.Request(subject, free, time.Second)
	if err != nil {
		t.Fatalf("the approve for another controller waited behind the held one: %v", err)
	}
	var resp bus.ApproveDeletionResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != "" {
		t.Fatalf("free reply = %s (decode error %v), want success", msg.Data, err)
	}

	close(h.release)
	if got := <-held; got == nil {
		t.Fatal("the held approve got no reply after its release")
	}
}
