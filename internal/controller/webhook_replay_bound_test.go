package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// --- replay give-up bound and consumer supervision ---

func TestDeliveryAge_FallsBackToReceivedAt(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	env := webhookEnvelope{ReceivedAt: now.Add(-5 * time.Minute).Format(time.RFC3339)}
	age, ok := deliveryAge(&nats.Msg{Data: []byte("{}")}, env, now)
	if !ok || age != 5*time.Minute {
		t.Fatalf("deliveryAge = (%v, %v), want (5m, true)", age, ok)
	}
}

func TestDeliveryAge_UnknownWithoutTimestamp(t *testing.T) {
	if _, ok := deliveryAge(&nats.Msg{}, webhookEnvelope{}, time.Now()); ok {
		t.Fatal("deliveryAge reported a known age for a message with no timestamp")
	}
}

func TestHandleQueuedWebhook_TermsTenantDeliveryPastReplayBound(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	env := webhookEnvelope{
		Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "old",
		ReceivedAt: time.Now().Add(-(replayGiveUpAge + time.Minute)).UTC().Format(time.RFC3339),
	}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if n := len(rt.calls()); n != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for a delivery past the replay bound", n)
	}
}

func TestHandleQueuedWebhook_TermsControllerDeliveryPastReplayBound(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	env := webhookEnvelope{
		Cluster: "core", Namespace: "ns1", Controller: "ctrl", Path: "generic-webhook-trigger/invoke",
		ReceivedAt: time.Now().Add(-(replayGiveUpAge + time.Minute)).UTC().Format(time.RFC3339),
	}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if n := len(rt.calls()); n != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for a delivery past the replay bound", n)
	}
}

func TestHandleQueuedWebhook_ReplaysTenantDeliveryInsideReplayBound(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	env := webhookEnvelope{
		Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "fresh",
		ReceivedAt: time.Now().Add(-(replayGiveUpAge - 2*time.Minute)).UTC().Format(time.RFC3339),
	}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if n := len(rt.calls()); n != 1 {
		t.Fatalf("SendImperative called %d times, want 1 for a delivery inside the replay bound", n)
	}
}

func TestSuperviseReplay_RerunsPassAfterFailureWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	passes := 0
	var waits []time.Duration
	pass := func(context.Context) error {
		passes++
		if passes < 3 {
			return errors.New("consumer gone")
		}
		cancel()
		return nil
	}
	wait := func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	superviseReplay(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "core", pass, wait, time.Now)

	if passes != 3 {
		t.Fatalf("passes = %d, want 3", passes)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v, want [1s 2s]", waits)
	}
}

func TestSuperviseReplay_ResetsBackoffAfterLongPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := time.Unix(0, 0)
	now := func() time.Time { return clock }
	passes := 0
	var waits []time.Duration
	pass := func(context.Context) error {
		passes++
		switch passes {
		case 1:
			return errors.New("fast failure") // short pass: backoff grows
		case 2:
			clock = clock.Add(replayRebuildResetAfter) // long pass: backoff resets
			return errors.New("slow failure")
		default:
			cancel()
			return nil
		}
	}
	wait := func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	superviseReplay(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "core", pass, wait, now)

	if len(waits) != 2 || waits[0] != time.Second || waits[1] != time.Second {
		t.Fatalf("waits = %v, want [1s 1s] after a long pass reset the backoff", waits)
	}
}

func TestSuperviseReplay_StopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passes := 0
	superviseReplay(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "core",
		func(context.Context) error {
			passes++
			return errors.New("fail")
		},
		func(context.Context, time.Duration) bool { return false },
		time.Now)
	if passes != 1 {
		t.Fatalf("passes = %d, want 1 when the wait reports the context ended", passes)
	}
}

func TestRunWebhookReplayPass_ReturnsWhenConsumerDeleted(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	opts := &server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	rec := newTestReconciler(newTestClient())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- rec.runWebhookReplayPass(ctx, conn, "core", 1, 200*time.Millisecond)
	}()

	// Wait until the pass has created the durable, then delete it from outside.
	durable := webhookReplayConsumer + "-core"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := conn.ConsumerInfo(bus.WebhookStreamName, durable); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replay durable was never created")
		}
		time.Sleep(20 * time.Millisecond)
	}
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("raw connect: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteConsumer(bus.WebhookStreamName, durable); err != nil {
		t.Fatalf("delete consumer: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pass returned nil after its consumer was deleted, want an error so the supervisor rebuilds")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pass did not return after its consumer was deleted")
	}
}

func TestPublishReplayRejected_ExpiredTenantDeliveryEvent(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	received := make(chan activity.Event, 4)
	sub, err := conn.SubscribeData(bus.ActivityGlobal("core"), func(data []byte) {
		var e activity.Event
		if json.Unmarshal(data, &e) == nil {
			received <- e
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	rec := newTestReconciler(newTestClient())
	rec.SetActivityPublisher(activity.NewPublisher("core", conn))
	rec.publishReplayRejected(webhookEnvelope{Tenant: "payments", Forge: "gitea", DeliveryID: "abc"},
		"delivery gitea:abc was not replayed within 50m0s; dropped")

	select {
	case e := <-received:
		if e.Type != "webhook.replay.rejected" || e.Severity != "warning" || e.Source != "operator" {
			t.Fatalf("event = %+v, want webhook.replay.rejected warning from operator", e)
		}
		if !strings.Contains(e.Message, "tenant payments:") || !strings.Contains(e.Message, "gitea:abc") {
			t.Fatalf("message = %q, want tenant name and delivery id", e.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook.replay.rejected event received")
	}
}
