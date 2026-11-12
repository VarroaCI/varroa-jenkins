package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// TestHandleConnected_StreamLossHoldsRunning asserts that a Connected
// controller whose mite stream stays lost moves to Running, not Pending. The
// pod is unchanged, so a reprovision pass does nothing but churn.
func TestHandleConnected_StreamLossHoldsRunning(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl", 0, "", nil)
	seen := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, LastSeen: &seen, JenkinsHealth: "healthy"}
	rec.miteTokens["ns/ctl"] = miteTokenEntry{}

	// No registry entry: the mite is disconnected. Ticks 1 and 2 are grace.
	for i := 1; i <= 2; i++ {
		_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Fatalf("tick %d: phase = %s, want Connected during grace", i, cr.Status.Phase)
		}
	}
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})

	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
		t.Fatalf("phase = %s, want Running", cr.Status.Phase)
	}
	if cr.Status.MiteStatus.Connected {
		t.Error("miteStatus.connected = true, want false after stream loss")
	}
	ready := findCondition(cr.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "MiteDisconnected" {
		t.Errorf("Ready = %+v, want False/MiteDisconnected", ready)
	}
	mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected)
	if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonStreamLost {
		t.Fatalf("MiteConnected = %+v, want False/StreamLost", mc)
	}
	if !strings.Contains(mc.Message, "ns/ctl") || !strings.Contains(mc.Message, "no presence record") {
		t.Errorf("MiteConnected message %q does not name the mite and the cause", mc.Message)
	}
	if _, ok := rec.miteTokens["ns/ctl"]; ok {
		t.Error("mite token record kept; a reconnect must receive a fresh token")
	}
}

// staleTransport decorates a Transport so Info reports a connected mite whose
// last heartbeat is older than staleMiteThreshold.
type staleTransport struct {
	transport.Transport
}

func (s *staleTransport) Info(_, _ string) (transport.MiteInfo, bool, error) {
	return transport.MiteInfo{Version: "v1.0", LastHeartbeat: time.Now().Add(-2 * staleMiteThreshold), CertExpiry: time.Now().Add(time.Hour)}, true, nil
}

// TestHandleRunning_StaleHeartbeatIsNotAReconnect asserts that handleRunning
// applies the same stale-heartbeat rule as handleConnected. A presence record
// with an aged heartbeat must not return a stream-lost controller to
// Connected, or the two handlers flap the phase.
func TestHandleRunning_StaleHeartbeatIsNotAReconnect(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)
	rec.miteTransport = &staleTransport{Transport: rec.miteTransport}

	cr := streamLostCtrl()
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
		t.Fatalf("phase = %s, want Running for a stale heartbeat", cr.Status.Phase)
	}

	client.stsContainerEnv = map[string]map[string]string{"mite": {"VARROA_CA_PEM": "old"}}
	if !rec.reconcileMiteTransportEnvRoll(context.Background(), cr) {
		t.Fatal("a stale heartbeat must not block the transport env roll")
	}
}

// streamLostCtrl returns a Running controller that lost its mite stream.
func streamLostCtrl() *v1alpha1.Controller {
	cr := runningCtrl("ctl")
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now().Add(-time.Minute))}
	return cr
}

// TestReconcileMiteTransportEnvRoll asserts that a stream loss rolls the pod
// only when the mite's transport env differs from the operator's values or
// the StatefulSet is gone.
func TestReconcileMiteTransportEnvRoll(t *testing.T) {
	const ca, endpoint = "CA-NEW", "gw.varroa:9090"
	cases := []struct {
		name     string
		cr       func() *v1alpha1.Controller
		env      map[string]map[string]string
		envErr   error
		wantRoll bool
	}{
		{"no stream loss", func() *v1alpha1.Controller { return runningCtrl("ctl") }, nil, nil, false},
		{"env matches", streamLostCtrl, map[string]map[string]string{"mite": {"VARROA_CA_PEM": ca, "VARROA_ENDPOINT": endpoint}}, nil, false},
		{"CA differs", streamLostCtrl, map[string]map[string]string{"mite": {"VARROA_CA_PEM": "CA-OLD", "VARROA_ENDPOINT": endpoint}}, nil, true},
		{"endpoint differs", streamLostCtrl, map[string]map[string]string{"mite": {"VARROA_CA_PEM": ca, "VARROA_ENDPOINT": "old:9090"}}, nil, true},
		{"StatefulSet missing", streamLostCtrl, nil, nil, true},
		{"read error", streamLostCtrl, nil, errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClientWithGateBundle()
			rec, _ := newTestReconcilerForGate(client)
			rec.caPEM = ca
			rec.varroaEndpoint = endpoint
			client.stsContainerEnv = tc.env
			client.stsContainerEnvErr = tc.envErr
			cr := tc.cr()

			got := rec.reconcileMiteTransportEnvRoll(context.Background(), cr)
			if got != tc.wantRoll {
				t.Fatalf("rolled = %v, want %v", got, tc.wantRoll)
			}
			if !tc.wantRoll {
				if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
					t.Fatalf("phase = %s, want Running", cr.Status.Phase)
				}
				return
			}
			if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
				t.Fatalf("phase = %s, want Provisioning", cr.Status.Phase)
			}
			if cr.Status.ProvisioningStartedAt == nil || time.Since(cr.Status.ProvisioningStartedAt.Time) > time.Minute {
				t.Fatalf("ProvisioningStartedAt = %v, want now", cr.Status.ProvisioningStartedAt)
			}
		})
	}
}

// TestCellReadyMiteUnreachable asserts that a cell in Running with a lost
// stream reports MiteUnreachable and still rolls after the grace period.
func TestCellReadyMiteUnreachable(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	cr.Status.Phase = v1alpha1.ControllerPhaseRunning
	cr.Status.Conditions = append(cr.Status.Conditions, streamLostCond(time.Now()))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	rec.evaluateCellReadiness(context.Background(), cr, t0)
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonMiteUnreachable {
		t.Fatalf("CellReady = %+v, want False/MiteUnreachable", cond)
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(grace+time.Minute))
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("expected 1 pod delete after the grace period, got %d", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled || cond.Message != v1alpha1.ReasonMiteUnreachable {
		t.Fatalf("CellReady = %+v, want CellRolled/MiteUnreachable", cond)
	}

	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(2*grace+time.Minute))
	if !cr.Status.Cell.Evacuate {
		t.Fatal("expected evacuate after twice the grace period")
	}
}

// presenceReadErrTransport decorates a Transport so Info fails to read the
// presence record, as a NATS timeout does.
type presenceReadErrTransport struct {
	transport.Transport
}

func (p *presenceReadErrTransport) Info(_, _ string) (transport.MiteInfo, bool, error) {
	return transport.MiteInfo{}, false, errors.New("kv get presence: nats: timeout")
}

// logCapture points the reconciler logger at a buffer for log assertions.
func logCapture(rec *Reconciler) *bytes.Buffer {
	var buf bytes.Buffer
	rec.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

// connectedCtrlSeenAgo returns a Connected controller whose last seen heartbeat
// is the given age in the past.
func connectedCtrlSeenAgo(age time.Duration) *v1alpha1.Controller {
	cr := newGateCtrl("ctl", 0, "", nil)
	seen := metav1.NewTime(time.Now().Add(-age))
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{Connected: true, LastSeen: &seen, JenkinsHealth: "healthy"}
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	return cr
}

// TestHandleConnected_PresenceReadErrorKeepsConnected asserts that a failed
// presence read does not start the stream-loss grace: the phase stays
// Connected, and no miss is counted.
func TestHandleConnected_PresenceReadErrorKeepsConnected(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)
	logs := logCapture(rec)
	cr := connectedCtrlSeenAgo(10 * time.Second)
	rec.miteTokens["ns/ctl"] = miteTokenEntry{}
	rec.miteTransport = &presenceReadErrTransport{Transport: rec.miteTransport}

	for i := 0; i < 5; i++ {
		_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	}

	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase = %s, want Connected", cr.Status.Phase)
	}
	if mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected); mc != nil && mc.Status == metav1.ConditionFalse {
		t.Errorf("MiteConnected = %+v, want no lost condition", mc)
	}
	if n := rec.disconnectedTicks["ns/ctl"]; n != 0 {
		t.Errorf("miss count = %d, want 0", n)
	}
	if _, ok := rec.miteTokens["ns/ctl"]; !ok {
		t.Error("mite token record dropped on a read error")
	}
	if !strings.Contains(logs.String(), "mite presence read failed; phase kept") {
		t.Errorf("log %q does not record the kept phase", logs.String())
	}
}

// TestHandleConnected_PresenceReadErrorKeepsMissCount asserts that a read
// error neither adds to nor resets the miss count of earlier missing passes.
func TestHandleConnected_PresenceReadErrorKeepsMissCount(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)
	cr := connectedCtrlSeenAgo(10 * time.Second)
	base := rec.miteTransport

	for i := 0; i < 2; i++ {
		_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	}
	if n := rec.disconnectedTicks["ns/ctl"]; n != 2 {
		t.Fatalf("miss count = %d, want 2", n)
	}

	rec.miteTransport = &presenceReadErrTransport{Transport: base}
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if n := rec.disconnectedTicks["ns/ctl"]; n != 2 {
		t.Fatalf("miss count after read error = %d, want 2", n)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase after read error = %s, want Connected", cr.Status.Phase)
	}

	rec.miteTransport = base
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
		t.Fatalf("phase = %s, want Running after the third miss", cr.Status.Phase)
	}
	if mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected); mc == nil || !strings.Contains(mc.Message, "no presence record") {
		t.Errorf("MiteConnected = %+v, want the missing cause", mc)
	}
}

// TestHandleConnected_StreamLossNamesCause asserts that a stream loss names
// its cause in the MiteConnected message and in the operator log.
func TestHandleConnected_StreamLossNamesCause(t *testing.T) {
	cases := []struct {
		name     string
		cause    string
		detail   string
		lastSeen time.Duration
		wrap     func(transport.Transport) transport.Transport
	}{
		{"missing", "missing", "no presence record", 10 * time.Second,
			func(b transport.Transport) transport.Transport { return b }},
		{"stale", "stale", "heartbeat older than", 10 * time.Second,
			func(b transport.Transport) transport.Transport { return &staleTransport{Transport: b} }},
		{"unreadable", "unreadable", "presence record unreadable", 3 * time.Minute,
			func(b transport.Transport) transport.Transport { return &presenceReadErrTransport{Transport: b} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClientWithGateBundle()
			rec, _ := newTestReconcilerForGate(client)
			logs := logCapture(rec)
			cr := connectedCtrlSeenAgo(tc.lastSeen)
			rec.miteTransport = tc.wrap(rec.miteTransport)

			for i := 0; i < 3; i++ {
				_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
			}

			if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
				t.Fatalf("phase = %s, want Running", cr.Status.Phase)
			}
			mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected)
			if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonStreamLost {
				t.Fatalf("MiteConnected = %+v, want False/StreamLost", mc)
			}
			if !strings.Contains(mc.Message, "ns/ctl") || !strings.Contains(mc.Message, tc.detail) {
				t.Errorf("MiteConnected message %q lacks the mite or the cause %q", mc.Message, tc.detail)
			}
			out := logs.String()
			if !strings.Contains(out, "mite stream lost; phase set to Running") || !strings.Contains(out, "cause="+tc.cause) {
				t.Errorf("log %q lacks the stream-loss line with cause=%s", out, tc.cause)
			}
		})
	}
}

// TestReconcileMiteTransportEnvRoll_PresenceReadError asserts that a failed
// presence read never drives a pod roll, even when the transport env differs.
func TestReconcileMiteTransportEnvRoll_PresenceReadError(t *testing.T) {
	const ca, endpoint = "CA-NEW", "gw.varroa:9090"
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)
	rec.caPEM = ca
	rec.varroaEndpoint = endpoint
	client.stsContainerEnv = map[string]map[string]string{"mite": {"VARROA_CA_PEM": "CA-OLD", "VARROA_ENDPOINT": endpoint}}
	rec.miteTransport = &presenceReadErrTransport{Transport: rec.miteTransport}
	logs := logCapture(rec)

	if rec.reconcileMiteTransportEnvRoll(context.Background(), streamLostCtrl()) {
		t.Fatal("rolled on a presence read error")
	}
	if !strings.Contains(logs.String(), "presence read failed") {
		t.Errorf("log %q does not record the skipped evaluation", logs.String())
	}
}

// TestPresenceMissCounter_Concurrent asserts that reconciles of different
// controllers may touch the miss counter at once.
func TestPresenceMissCounter_Concurrent(t *testing.T) {
	rec := &Reconciler{disconnectedTicks: make(map[string]int)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("ns/c%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				rec.countPresenceMiss(key)
				rec.resetPresenceMisses(key)
			}
		}()
	}
	wg.Wait()
	if len(rec.disconnectedTicks) != 0 {
		t.Errorf("counter holds %d keys after reset, want 0", len(rec.disconnectedTicks))
	}
}

// TestPhaseEventReason asserts that only a Connected to Running stream loss
// carries the MiteConnected message as the activity reason.
func TestPhaseEventReason(t *testing.T) {
	lost := streamLostCtrl()
	want := "mite ns/ctl command stream lost: missing; last heartbeat never"
	lost.Status.Conditions = setCondition(lost.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteConnected,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ReasonStreamLost,
		Message: want,
	})
	if got := phaseEventReason(lost, v1alpha1.ControllerPhaseConnected); got != want {
		t.Errorf("stream loss reason = %q, want %q", got, want)
	}
	if got := phaseEventReason(lost, v1alpha1.ControllerPhaseProvisioning); got != "" {
		t.Errorf("provisioning-to-running reason = %q, want empty", got)
	}
	if got := phaseEventReason(runningCtrl("ctl"), v1alpha1.ControllerPhaseConnected); got != "" {
		t.Errorf("connected-to-running without a stream loss reason = %q, want empty", got)
	}
}
