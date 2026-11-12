package controller

import (
	"context"
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// holdTestWait is how long a test waits for an event that should not happen.
const holdTestWait = 5 * time.Second

// holdSetup builds a Reconciler whose sends succeed, with the Connected controller
// ns1/ctrl. The result wait and the probe interval are set by the caller.
func holdSetup(t *testing.T) (*Reconciler, *crdstore.Fake, *recordingTransport, *replayWaiters) {
	t.Helper()
	rec, store, rt := newTenantWebhookTestReconciler(t)
	rt.err = nil
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})
	return rec, store, rt, newReplayWaiters()
}

// holdControllerMsg is a controller-shape delivery with no stream metadata, so
// its command id is "0".
func holdControllerMsg(t *testing.T, receivedAt string) *nats.Msg {
	t.Helper()
	return tenantEnvelopeMsg(t, webhookEnvelope{
		Cluster: "core", Namespace: "ns1", Controller: "ctrl", Path: "github-webhook/",
		BodyB64:    base64.StdEncoding.EncodeToString([]byte("payload")),
		ReceivedAt: receivedAt,
	})
}

// runHeldHandler runs the handler in a goroutine. The returned channel closes
// when the handler returns.
func runHeldHandler(ctx context.Context, rec *Reconciler, msg *nats.Msg, waiters *replayWaiters) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.handleQueuedWebhook(ctx, "core", msg, waiters)
	}()
	return done
}

// answerHeld delivers the result of the command with id "0" to the handler.
func answerHeld(waiters *replayWaiters) {
	waiters.deliver(&mitev1.CommandResult{CommandId: "0", HttpStatus: 200})
}

// awaitDone waits until done closes, or fails the test.
func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(holdTestWait):
		t.Fatal("handler did not return")
	}
}

// requireNotDone fails the test when done has closed.
func requireNotDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("handler returned while its replay was held")
	default:
	}
}

// awaitSends waits until rt has recorded at least n sends.
func awaitSends(t *testing.T, rt *recordingTransport, n int) {
	t.Helper()
	deadline := time.Now().Add(holdTestWait)
	for len(rt.calls()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("sends = %d, want at least %d", len(rt.calls()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHandleQueuedWebhook_HeldReplaySendsNothingUntilItsResult(t *testing.T) {
	rec, _, rt, waiters := holdSetup(t)
	rec.replayResultWait = 20 * time.Millisecond
	rec.replayProbeEvery = time.Hour

	done := runHeldHandler(context.Background(), rec, holdControllerMsg(t, ""), waiters)
	time.Sleep(200 * time.Millisecond)
	requireNotDone(t, done)
	if got := len(rt.calls()); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}

	answerHeld(waiters)
	awaitDone(t, done)
	if got := len(rt.calls()); got != 1 {
		t.Fatalf("sends after the answer = %d, want 1", got)
	}
}

func TestHandleQueuedWebhook_HeldReplayResendsSameCommandWhileConnected(t *testing.T) {
	rec, _, rt, waiters := holdSetup(t)
	rec.replayResultWait = 10 * time.Millisecond
	rec.replayProbeEvery = 20 * time.Millisecond

	done := runHeldHandler(context.Background(), rec, holdControllerMsg(t, ""), waiters)
	awaitSends(t, rt, 3)
	answerHeld(waiters)
	awaitDone(t, done)

	calls := rt.calls()
	for i, c := range calls {
		if c.ns != "ns1" || c.name != "ctrl" {
			t.Fatalf("send %d target = %s/%s, want ns1/ctrl", i, c.ns, c.name)
		}
		if c.cmd.CommandId != calls[0].cmd.CommandId {
			t.Fatalf("send %d command id = %q, want the first id %q", i, c.cmd.CommandId, calls[0].cmd.CommandId)
		}
	}
}

func TestHandleQueuedWebhook_HeldReplaySkipsResendWhileNotConnected(t *testing.T) {
	rec, store, rt, waiters := holdSetup(t)
	rec.replayResultWait = 10 * time.Millisecond
	rec.replayProbeEvery = 10 * time.Millisecond
	rt.onSend = func(n int) {
		if n == 1 {
			crdstore.MustSeed(store, &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"},
				Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseRunning},
			})
		}
	}

	done := runHeldHandler(context.Background(), rec, holdControllerMsg(t, ""), waiters)
	time.Sleep(200 * time.Millisecond)
	requireNotDone(t, done)
	if got := len(rt.calls()); got != 1 {
		t.Fatalf("sends = %d, want 1 while the cell is not Connected", got)
	}

	answerHeld(waiters)
	awaitDone(t, done)
}

func TestHandleQueuedWebhook_HeldTenantReplayReleasesOnMove(t *testing.T) {
	rec, store, rt, waiters := holdSetup(t)
	rec.replayResultWait = 10 * time.Millisecond
	rec.replayProbeEvery = 10 * time.Millisecond
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "ctrl", CellNamespace: "ns1"},
		},
	})
	rt.onSend = func(n int) {
		if n == 1 {
			crdstore.MustSeed(store, &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{Name: "payments"},
				Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
				Status: v1alpha1.TenantStatus{
					Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-b", CellNamespace: "ns2"},
				},
			})
		}
	}

	env := webhookEnvelope{
		Cluster: "core", Tenant: "payments", Path: "github-webhook/",
		BodyB64: base64.StdEncoding.EncodeToString([]byte("payload")),
		Forge:   "github", DeliveryID: "d1",
	}
	done := runHeldHandler(context.Background(), rec, tenantEnvelopeMsg(t, env), waiters)
	awaitDone(t, done)

	calls := rt.calls()
	if len(calls) != 1 {
		t.Fatalf("sends = %d, want 1", len(calls))
	}
	if calls[0].ns != "ns1" || calls[0].name != "ctrl" {
		t.Fatalf("target = %s/%s, want ns1/ctrl", calls[0].ns, calls[0].name)
	}
}

func TestHandleQueuedWebhook_HeldReplayGivesUpAtReplayBound(t *testing.T) {
	rec, _, rt, waiters := holdSetup(t)
	rec.replayResultWait = 10 * time.Millisecond
	rec.replayProbeEvery = time.Hour
	receivedAt := time.Now().Add(-replayGiveUpAge + 300*time.Millisecond).UTC().Format(time.RFC3339Nano)

	done := runHeldHandler(context.Background(), rec, holdControllerMsg(t, receivedAt), waiters)
	awaitDone(t, done)
	if got := len(rt.calls()); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
}

func TestHeldReplayCheck(t *testing.T) {
	tenantEnv := webhookEnvelope{Cluster: "core", Tenant: "payments", Forge: "github", DeliveryID: "d1"}
	ctrlEnv := webhookEnvelope{Cluster: "core", Namespace: "ns1", Controller: "ctrl"}
	placed := func(cell, ns string) *v1alpha1.TenantPlacementStatus {
		return &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: cell, CellNamespace: ns}
	}
	moving := []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TenantReasonTenantMoving,
	}}
	ctrl := func(phase v1alpha1.ControllerPhase) *v1alpha1.Controller {
		return &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"},
			Status:     v1alpha1.ControllerStatus{Phase: phase},
		}
	}
	tenant := func(p *v1alpha1.TenantPlacementStatus, conds []v1alpha1.TenantCondition) *v1alpha1.Tenant {
		return &v1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "payments"},
			Status:     v1alpha1.TenantStatus{Placement: p, Conditions: conds},
		}
	}

	cases := []struct {
		name    string
		env     webhookEnvelope
		objects []any
		want    heldReplayAction
	}{
		{"controller connected", ctrlEnv, []any{ctrl(v1alpha1.ControllerPhaseConnected)}, holdResend},
		{"controller running", ctrlEnv, []any{ctrl(v1alpha1.ControllerPhaseRunning)}, holdWait},
		{"controller absent", ctrlEnv, nil, holdRelease},
		{"tenant on the same cell", tenantEnv,
			[]any{tenant(placed("ctrl", "ns1"), nil), ctrl(v1alpha1.ControllerPhaseConnected)}, holdResend},
		{"tenant moved", tenantEnv,
			[]any{tenant(placed("cell-b", "ns2"), nil), ctrl(v1alpha1.ControllerPhaseConnected)}, holdRelease},
		{"tenant moving", tenantEnv,
			[]any{tenant(placed("ctrl", "ns1"), moving), ctrl(v1alpha1.ControllerPhaseConnected)}, holdRelease},
		{"tenant deleted", tenantEnv, []any{ctrl(v1alpha1.ControllerPhaseConnected)}, holdRelease},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, store, _ := newTenantWebhookTestReconciler(t)
			crdstore.MustSeed(store, tc.objects...)
			if got := rec.heldReplayCheck(context.Background(), tc.env, "ns1", "ctrl"); got != tc.want {
				t.Fatalf("action = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPastReplayBound(t *testing.T) {
	now := time.Now()
	ackedAt := func(ts time.Time) *nats.Msg {
		return &nats.Msg{
			Sub:   &nats.Subscription{},
			Reply: "$JS.ACK.varroa_webhooks.c.1.5.5." + strconv.FormatInt(ts.UnixNano(), 10) + ".0",
		}
	}
	cases := []struct {
		name string
		msg  *nats.Msg
		want bool
	}{
		{"old", ackedAt(now.Add(-replayGiveUpAge - time.Minute)), true},
		{"young", ackedAt(now.Add(-time.Minute)), false},
		{"no reply", &nats.Msg{Sub: &nats.Subscription{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pastReplayBound(tc.msg, now); got != tc.want {
				t.Fatalf("pastReplayBound = %v, want %v", got, tc.want)
			}
		})
	}
}
