package recordstore

import "testing"

// TestWaitNotificationBody asserts the wait notification carries the run
// identity and its wait state, and no lock field.
func TestWaitNotificationBody(t *testing.T) {
	req := ClaimRequest{Cluster: testCluster, Tenant: testTenant, JobPath: testJobPath}
	n := waitNotification(req, "run-1", WaitStateWaiting)
	body := marshalNotification(t, n)

	if got := body["kind"]; got != "wait" {
		t.Errorf("kind = %v, want wait", got)
	}
	if got := body["state"]; got != WaitStateWaiting {
		t.Errorf("state = %v, want %s", got, WaitStateWaiting)
	}
	if got := body["cluster"]; got != testCluster {
		t.Errorf("cluster = %v, want %s", got, testCluster)
	}
	if got := body["tenant"]; got != testTenant {
		t.Errorf("tenant = %v, want %s", got, testTenant)
	}
	if got := body["job_path"]; got != testJobPath {
		t.Errorf("job_path = %v, want %s", got, testJobPath)
	}
	if got := body["run_id"]; got != "run-1" {
		t.Errorf("run_id = %v, want run-1", got)
	}
	if _, ok := body["lock_name"]; ok {
		t.Errorf("a wait notification carries lock_name: %v", body)
	}

	notifier, capture, _ := newCaptureNotifier(t)
	notifier.Notify(n)
	notifier.Close()
	if got := capture.count(); got != 1 {
		t.Fatalf("published %d notifications, want 1", got)
	}
	if got := capture.subject(0); got != "runs."+testCluster+"."+testTenant {
		t.Errorf("subject = %s, want runs.%s.%s", got, testCluster, testTenant)
	}
}
