package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRunnerDeepCopyIsolatesSubObjects mutates a deep copy of a populated
// Runner and asserts the original is unchanged: the round-trip acceptance
// for task 2.2's hand-added DeepCopy set.
func TestRunnerDeepCopyIsolatesSubObjects(t *testing.T) {
	port := 2222
	lastSeen := metav1.Now()
	orig := Runner{
		Spec: RunnerSpec{
			Tenant:   "payments",
			Launcher: "ssh",
			Labels:   []string{"windows", "msbuild"},
			RemoteFS: "/home/jenkins",
			Host:     "10.0.0.5",
			Port:     &port,
		},
		Status: RunnerStatus{
			NodeName:      "payments-win-1",
			Cell:          "cell-a",
			CellNamespace: "varroa-system",
			Connected:     true,
			LastSeen:      &lastSeen,
			Conditions: []RunnerCondition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Connected"},
			},
		},
	}

	cp := orig.DeepCopy()

	cp.Spec.Labels[0] = "mutated"
	*cp.Spec.Port = 9999
	cp.Status.Conditions[0].Reason = "Mutated"
	*cp.Status.LastSeen = metav1.NewTime(lastSeen.Add(1))
	cp.Status.NodeName = "mutated"

	if got := orig.Spec.Labels[0]; got != "windows" {
		t.Fatalf("labels shallow-copied: original Labels[0] = %q, want windows", got)
	}
	if got := *orig.Spec.Port; got != 2222 {
		t.Fatalf("port shallow-copied: original Port = %d, want 2222", got)
	}
	if got := orig.Status.Conditions[0].Reason; got != "Connected" {
		t.Fatalf("conditions shallow-copied: original Reason = %q, want Connected", got)
	}
	if !orig.Status.LastSeen.Equal(&lastSeen) {
		t.Fatalf("lastSeen shallow-copied: original LastSeen = %v, want %v", orig.Status.LastSeen, lastSeen)
	}
	if got := orig.Status.NodeName; got != "payments-win-1" {
		t.Fatalf("nodeName field shallow-copied: original NodeName = %q, want payments-win-1", got)
	}
}

// TestRunnerListDeepCopyIsolatesItems mutates a deep copy of a RunnerList's
// items and asserts the original list is unchanged.
func TestRunnerListDeepCopyIsolatesItems(t *testing.T) {
	orig := RunnerList{
		Items: []Runner{
			{Spec: RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/home/jenkins"}},
		},
	}

	cp := orig.DeepCopy()
	cp.Items[0].Spec.Tenant = "mutated"

	if got := orig.Items[0].Spec.Tenant; got != "payments" {
		t.Fatalf("items shallow-copied: original Items[0].Spec.Tenant = %q, want payments", got)
	}
}
