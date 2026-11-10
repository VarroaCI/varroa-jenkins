package forgeops

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const statusJobURL = "https://jenkins.example.org/job/payments/job/app/job/main/display/redirect"

// resultReq returns a request that ends a completed run with the given result.
func resultReq(result string) forgeapi.CommitStatusRequest {
	req := endReq()
	req.Reason = ""
	req.Result = result
	return req
}

// TestEndPendingStatus_ResultSetsMappedState checks the state and the
// description that each Jenkins result writes.
func TestEndPendingStatus_ResultSetsMappedState(t *testing.T) {
	cases := map[string]string{
		"SUCCESS":   forge.StateSuccess,
		"UNSTABLE":  forge.StateFailure,
		"FAILURE":   forge.StateFailure,
		"ABORTED":   forge.StateError,
		"NOT_BUILT": forge.StateError,
	}
	for result, wantState := range cases {
		t.Run(result, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			c := pushable()
			c.statuses = []forge.CommitStatus{
				{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusBuildURL},
			}
			oneWriter(fx, c)
			n, err := fx.ops.EndPendingStatus(context.Background(), resultReq(result))
			if err != nil || n != 1 {
				t.Fatalf("written %d, err %v; want 1, nil", n, err)
			}
			w := c.statusWrites[0]
			if w.State != wantState {
				t.Fatalf("state = %q, want %q", w.State, wantState)
			}
			if w.Description != "Varroa reported the build result: "+result {
				t.Fatalf("description = %q", w.Description)
			}
		})
	}
}

// TestEndPendingStatus_ResultEndsQueuedJobStatus checks that a Result request
// also ends the queued status whose target URL names the job itself.
func TestEndPendingStatus_ResultEndsQueuedJobStatus(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusJobURL, Description: "Build queued..."},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), resultReq("FAILURE"))
	if err != nil || n != 1 {
		t.Fatalf("written %d, err %v; want 1, nil", n, err)
	}
	w := c.statusWrites[0]
	if w.State != forge.StateFailure || w.TargetURL != statusJobURL {
		t.Fatalf("write = %+v", w)
	}
}

// TestEndPendingStatus_ReasonSkipsQueuedJobStatus checks that a Reason request
// leaves the queued status of the job URL alone, because an automatic retry
// owns that status.
func TestEndPendingStatus_ReasonSkipsQueuedJobStatus(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusJobURL},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 0 || len(c.statusWrites) != 0 {
		t.Fatalf("written %d, writes %d, err %v; want none", n, len(c.statusWrites), err)
	}
}

// TestJobURLMatches covers the job URL forms that a queued status carries.
func TestJobURLMatches(t *testing.T) {
	cases := []struct {
		name   string
		target string
		job    string
		want   bool
	}{
		{"plain job url", "https://j.example.org/job/payments/job/app/job/main/", "payments/app/main", true},
		{"display redirect", statusJobURL, "payments/app/main", true},
		{"headless prefix", "https://j.example.org/jenkins/job/payments/job/app/job/main/display/redirect", "payments/app/main", true},
		{"encoded branch slash", "https://j.example.org/job/payments/job/app/job/feature%2Fx/display/redirect", "payments/app/feature%2Fx", true},
		{"doubly encoded branch slash", "https://j.example.org/job/payments/job/app/job/feature%252Fx/", "payments/app/feature%2Fx", true},
		{"build url", statusBuildURL, "payments/app/main", false},
		{"build url with redirect", "https://j.example.org/job/payments/job/app/job/main/7/display/redirect", "payments/app/main", false},
		{"parent folder", "https://j.example.org/job/payments/job/app/", "payments/app/main", false},
		{"another job", "https://j.example.org/job/payments/job/other/job/main/", "payments/app/main", false},
		{"nested job with the same tail", "https://j.example.org/job/other/job/payments/job/app/job/main/", "payments/app/main", false},
		{"url that does not parse", "https://[::1/job/payments/job/app/job/main/", "payments/app/main", false},
		{"empty target", "", "payments/app/main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobURLMatches(tc.target, tc.job); got != tc.want {
				t.Fatalf("jobURLMatches(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}
