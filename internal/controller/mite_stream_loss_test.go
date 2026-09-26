package controller

import (
	"context"
	"errors"
	"strings"
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
	if !strings.Contains(mc.Message, "ns/ctl") {
		t.Errorf("MiteConnected message %q does not name the mite", mc.Message)
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

func (s *staleTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "v1.0", time.Now().Add(-2 * staleMiteThreshold), time.Now().Add(time.Hour), true
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
