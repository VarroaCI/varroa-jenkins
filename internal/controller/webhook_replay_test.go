package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

func TestReplayAckDecision(t *testing.T) {
	cases := []struct {
		status int
		want   replayAction
	}{
		{200, ackReplay},
		{201, ackReplay},
		{202, ackReplay},
		{299, ackReplay},
		{0, nakReplay},   // transport-level failure (no HTTP response)
		{429, nakReplay}, // rate limited
		{500, nakReplay},
		{503, nakReplay},
		{400, termReplay}, // bad request
		{401, termReplay}, // unauthorized (bad signature)
		{404, termReplay},
		{422, termReplay},
	}
	for _, tc := range cases {
		if got := replayAckDecision(tc.status); got != tc.want {
			t.Errorf("replayAckDecision(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestReplayWaiters_Correlation(t *testing.T) {
	w := &replayWaiters{chans: make(map[string]chan *mitev1.CommandResult)}

	ch := w.register("cmd-7")
	// A result for a different command_id must not be delivered here.
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-other", HttpStatus: 500})
	select {
	case <-ch:
		t.Fatal("received a result for the wrong command_id")
	default:
	}

	// The matching result is delivered.
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-7", HttpStatus: 200})
	select {
	case res := <-ch:
		if res.HttpStatus != 200 {
			t.Errorf("got status %d, want 200", res.HttpStatus)
		}
	default:
		t.Fatal("matching result was not delivered")
	}

	// After unregister, delivery is a no-op (and must not panic).
	w.unregister("cmd-7")
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-7", HttpStatus: 200})
}

// TestWebhookEnvelope_MatchesBFFShape guards the operator's envelope decoder
// against the exact JSON the BFF writes (internal/api/handlers_hibernation.go).
func TestWebhookEnvelope_MatchesBFFShape(t *testing.T) {
	raw := `{
		"method": "POST",
		"path": "github-webhook/",
		"query": "",
		"headers": {"X-GitHub-Event": "push", "Content-Type": "application/json"},
		"bodyB64": "` + base64.StdEncoding.EncodeToString([]byte(`{"ref":"refs/heads/main"}`)) + `",
		"receivedAt": "2026-07-06T00:00:00Z",
		"cluster": "core",
		"namespace": "team-a",
		"controller": "demo"
	}`
	var env webhookEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Method != "POST" || env.Path != "github-webhook/" || env.Controller != "demo" || env.Namespace != "team-a" {
		t.Errorf("decoded envelope mismatch: %+v", env)
	}
	body, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil || string(body) != `{"ref":"refs/heads/main"}` {
		t.Errorf("body decode = %q (err %v)", body, err)
	}
	if env.Headers["X-GitHub-Event"] != "push" {
		t.Errorf("headers not decoded: %+v", env.Headers)
	}
}

// --- 11.2a: tenant envelope routing ---

// recordingTransport decorates a Transport so a test can record every
// SendImperative call and control its outcome, without standing up a real
// mite connection.
type recordingTransport struct {
	transport.Transport
	mu   sync.Mutex
	sent []recordedSend
	err  error
}

type recordedSend struct {
	ns, name string
	cmd      *mitev1.ImperativeCommand
}

func (rt *recordingTransport) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	rt.mu.Lock()
	rt.sent = append(rt.sent, recordedSend{ns: ns, name: name, cmd: cmd})
	rt.mu.Unlock()
	return rt.err
}

func (rt *recordingTransport) calls() []recordedSend {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]recordedSend, len(rt.sent))
	copy(out, rt.sent)
	return out
}

// newTenantWebhookTestReconciler builds a Reconciler whose SendImperative
// always fails (no mite connected), so a test can assert routing —
// whether SendImperative was called at all, and with which target —
// without waiting on the 30s result timeout.
func newTenantWebhookTestReconciler(t *testing.T) (*Reconciler, *crdstore.Fake, *recordingTransport) {
	t.Helper()
	client := newTestClient()
	rec := newTestReconciler(client)
	rt := &recordingTransport{Transport: rec.miteTransport, err: errors.New("no mite connected")}
	rec.miteTransport = rt
	return rec, client.store, rt
}

func tenantEnvelopeMsg(t *testing.T, env webhookEnvelope) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return &nats.Msg{Data: data}
}

func newReplayWaiters() *replayWaiters {
	return &replayWaiters{chans: make(map[string]chan *mitev1.CommandResult)}
}

func TestHandleQueuedWebhook_TenantNakEmptyPlacement(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
	})

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if len(rt.calls()) != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for an unplaced tenant", len(rt.calls()))
	}
}

func TestHandleQueuedWebhook_TenantNakTenantMoving(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
			Conditions: []v1alpha1.TenantCondition{
				{Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TenantReasonTenantMoving},
			},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if len(rt.calls()) != 0 {
		t.Fatalf("SendImperative called %d times, want 0 while the tenant is moving", len(rt.calls()))
	}
}

func TestHandleQueuedWebhook_TenantNakUnreadableController(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	})
	// No Controller seeded at all: absent counts the same as unreadable.

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if len(rt.calls()) != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for an absent Controller", len(rt.calls()))
	}
}

func TestHandleQueuedWebhook_TenantNakControllerNotConnected(t *testing.T) {
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
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseProvisioning},
	})

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if len(rt.calls()) != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for a not-Connected Controller", len(rt.calls()))
	}
}

func TestHandleQueuedWebhook_TenantTermDeletedTenant(t *testing.T) {
	rec, _, rt := newTenantWebhookTestReconciler(t)
	// No Tenant seeded at all.

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	if len(rt.calls()) != 0 {
		t.Fatalf("SendImperative called %d times, want 0 for a deleted Tenant", len(rt.calls()))
	}
}

func TestHandleQueuedWebhook_TenantSendsToCurrentPlacementAndJoinsDeliveryID(t *testing.T) {
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
		Cluster: "core", Tenant: "payments", Path: "github-webhook/",
		BodyB64: base64.StdEncoding.EncodeToString([]byte("payload")),
		Forge:   "github", DeliveryID: "8f2c",
	}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	calls := rt.calls()
	if len(calls) != 1 {
		t.Fatalf("SendImperative called %d times, want 1", len(calls))
	}
	if calls[0].ns != "ns1" || calls[0].name != "cell-a" {
		t.Fatalf("target = %s/%s, want ns1/cell-a", calls[0].ns, calls[0].name)
	}
	if got := calls[0].cmd.ReplayWebhook.DeliveryId; got != "github:8f2c" {
		t.Fatalf("ReplayWebhookPayload.delivery_id = %q, want github:8f2c", got)
	}
	if calls[0].cmd.CommandId == "" || calls[0].cmd.CommandId == calls[0].cmd.ReplayWebhook.DeliveryId {
		t.Fatalf("command_id = %q, want a distinct correlation id (the JetStream sequence), not the forge-joined delivery id", calls[0].cmd.CommandId)
	}
}

func TestHandleQueuedWebhook_TenantMoveRetargetsToCurrentPlacement(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1", "ns2"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-b", Namespace: "ns2"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	// The tenant moves to cell-b before this delivery is retried.
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1", "ns2"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-b", CellNamespace: "ns2"},
		},
	})
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	calls := rt.calls()
	if len(calls) != 2 {
		t.Fatalf("SendImperative called %d times, want 2", len(calls))
	}
	if calls[0].ns != "ns1" || calls[0].name != "cell-a" {
		t.Fatalf("first delivery target = %s/%s, want ns1/cell-a", calls[0].ns, calls[0].name)
	}
	if calls[1].ns != "ns2" || calls[1].name != "cell-b" {
		t.Fatalf("second delivery target = %s/%s, want the current placement ns2/cell-b", calls[1].ns, calls[1].name)
	}
}

// moveOnSecondTenantRead wraps a crdstore.Backend and rewrites the named
// Tenant's placement the second time its GetObject is called, so a test can
// land a move inside a single handleQueuedTenantWebhook invocation — in
// the gap between the checks-time read and the pre-send re-read — without
// a second, separate delivery.
type moveOnSecondTenantRead struct {
	crdstore.Backend
	store        *crdstore.Fake
	tenantName   string
	newPlacement *v1alpha1.TenantPlacementStatus
	calls        int
}

func newMoveOnSecondTenantRead(t *testing.T, store *crdstore.Fake, tenantName string, newPlacement *v1alpha1.TenantPlacementStatus) *moveOnSecondTenantRead {
	t.Helper()
	if _, err := crdstore.GVRFor[v1alpha1.Tenant](); err != nil {
		t.Fatalf("GVRFor[Tenant]: %v", err)
	}
	return &moveOnSecondTenantRead{
		Backend:      store,
		store:        store,
		tenantName:   tenantName,
		newPlacement: newPlacement,
	}
}

func (m *moveOnSecondTenantRead) GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	tenantGVR, _ := crdstore.GVRFor[v1alpha1.Tenant]()
	if gvr == tenantGVR && name == m.tenantName {
		m.calls++
		if m.calls == 2 {
			tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, m.store, m.tenantName, "")
			if err == nil {
				tenant.Status.Placement = m.newPlacement
				crdstore.MustSeed(m.store, tenant)
			}
		}
	}
	return m.Backend.GetObject(ctx, gvr, namespace, name)
}

// moveToTenantMovingOnSecondTenantRead wraps a crdstore.Backend and rewrites
// the named Tenant's Placed condition to False/TenantMoving the second time
// its GetObject is called, keeping the stored placement unchanged. It lets
// a test land a voluntary-move drain start inside a single
// handleQueuedWebhook invocation — in the gap between the checks-time read
// and the pre-send re-read — without changing the placement target itself.
type moveToTenantMovingOnSecondTenantRead struct {
	crdstore.Backend
	store      *crdstore.Fake
	tenantName string
	calls      int
}

func newMoveToTenantMovingOnSecondTenantRead(t *testing.T, store *crdstore.Fake, tenantName string) *moveToTenantMovingOnSecondTenantRead {
	t.Helper()
	if _, err := crdstore.GVRFor[v1alpha1.Tenant](); err != nil {
		t.Fatalf("GVRFor[Tenant]: %v", err)
	}
	return &moveToTenantMovingOnSecondTenantRead{
		Backend:    store,
		store:      store,
		tenantName: tenantName,
	}
}

func (m *moveToTenantMovingOnSecondTenantRead) GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	tenantGVR, _ := crdstore.GVRFor[v1alpha1.Tenant]()
	if gvr == tenantGVR && name == m.tenantName {
		m.calls++
		if m.calls == 2 {
			tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, m.store, m.tenantName, "")
			if err == nil {
				tenant.Status.Conditions = []v1alpha1.TenantCondition{
					{Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TenantReasonTenantMoving},
				}
				crdstore.MustSeed(m.store, tenant)
			}
		}
	}
	return m.Backend.GetObject(ctx, gvr, namespace, name)
}

// TestHandleQueuedWebhook_TenantSendRacesTenantMovingNaksInsteadOfOldCell
// proves the pre-send re-read also catches the earlier TenantMoving stage
// of a voluntary move (design.md D4's three-step move order), not only a
// later change of the placement target. The Tenant keeps the same Cell and
// CellNamespace throughout — only the Placed condition changes — so a
// re-read that compares Cell/CellNamespace alone would miss this race and
// dispatch the delivery to the draining old cell.
func TestHandleQueuedWebhook_TenantSendRacesTenantMovingNaksInsteadOfOldCell(t *testing.T) {
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

	// The wrapped store sets Placed=False/TenantMoving on the Tenant the
	// second time this handler reads it — the pre-send re-read — while
	// leaving Cell and CellNamespace exactly as they were at the
	// checks-time read.
	racy := newMoveToTenantMovingOnSecondTenantRead(t, store, "payments")
	rec.store = racy

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	calls := rt.calls()
	if len(calls) != 0 {
		t.Fatalf("SendImperative called %d times, want 0: a mid-flight TenantMoving transition must nak, never dispatch to the draining cell (%s/%s)",
			len(calls), calls[0].ns, calls[0].name)
	}
	if racy.calls < 2 {
		t.Fatalf("Tenant was read %d times, want at least 2 (checks-time read + pre-send re-read) for this race to be exercised", racy.calls)
	}
}

// TestHandleQueuedWebhook_TenantSendRacesMoveNaksInsteadOfOldCell proves
// 11.2a's fix: when the Tenant's placement changes in the gap between the
// checks-time read and the pre-send re-read — the exact race a single
// stale read would miss — the delivery is nak'd for redelivery and never
// reaches the cell the tenant already left.
func TestHandleQueuedWebhook_TenantSendRacesMoveNaksInsteadOfOldCell(t *testing.T) {
	rec, store, rt := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1", "ns2"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-b", Namespace: "ns2"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})

	// The wrapped store moves the tenant to cell-b the second time this
	// handler reads the Tenant — i.e. at the pre-send re-read — simulating
	// a move that lands after the checks above already passed against
	// cell-a.
	racy := newMoveOnSecondTenantRead(t, store, "payments", &v1alpha1.TenantPlacementStatus{
		Cluster: "core", Cell: "cell-b", CellNamespace: "ns2",
	})
	rec.store = racy

	env := webhookEnvelope{Cluster: "core", Tenant: "payments", Path: "github-webhook/", Forge: "github", DeliveryID: "d1"}
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())

	calls := rt.calls()
	if len(calls) != 0 {
		t.Fatalf("SendImperative called %d times, want 0: a mid-flight move must nak, never dispatch to the cell (%s/%s) the tenant already left",
			len(calls), calls[0].ns, calls[0].name)
	}
	if racy.calls < 2 {
		t.Fatalf("Tenant was read %d times, want at least 2 (checks-time read + pre-send re-read) for this race to be exercised", racy.calls)
	}

	// The redelivery, with the store now settled on the new placement,
	// reaches the new cell.
	rec.handleQueuedWebhook(context.Background(), "core", tenantEnvelopeMsg(t, env), newReplayWaiters())
	calls = rt.calls()
	if len(calls) != 1 {
		t.Fatalf("SendImperative called %d times on redelivery, want 1", len(calls))
	}
	if calls[0].ns != "ns2" || calls[0].name != "cell-b" {
		t.Fatalf("redelivery target = %s/%s, want ns2/cell-b", calls[0].ns, calls[0].name)
	}
}
