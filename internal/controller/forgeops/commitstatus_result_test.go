package forgeops

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// statusJobURL is the URL of the job payments/app/main under statusRoot.
const statusJobURL = statusRoot + "job/payments/job/app/job/main/display/redirect"

// resultReq returns a request that ends a completed run with the given result.
func resultReq(result string) forgeapi.CommitStatusRequest {
	req := endReq()
	req.Reason = ""
	req.Result = result
	return req
}

// pluginResultStates holds, for each forge kind, the state that the
// branch-source plugin of that forge writes for each Jenkins result. The
// gitea plugin writes skipped or warning for NOT_BUILT; the operator writes
// error, because it reads no forge version.
var pluginResultStates = map[forge.Kind]map[string]string{
	forge.KindGitHub: {
		"SUCCESS":   forge.StateSuccess,
		"UNSTABLE":  forge.StateFailure,
		"FAILURE":   forge.StateError,
		"ABORTED":   forge.StateError,
		"NOT_BUILT": forge.StateError,
	},
	forge.KindGitea: {
		"SUCCESS":   forge.StateSuccess,
		"UNSTABLE":  forge.StateFailure,
		"FAILURE":   forge.StateFailure,
		"ABORTED":   forge.StateError,
		"NOT_BUILT": forge.StateError,
	},
}

// TestEndPendingStatus_ResultSetsMappedState checks the state and the
// description that each Jenkins result writes on each forge kind.
func TestEndPendingStatus_ResultSetsMappedState(t *testing.T) {
	for kind, states := range pluginResultStates {
		for result, wantState := range states {
			t.Run(string(kind)+" "+result, func(t *testing.T) {
				fx := newFixture(t, "agents-payments")
				fx.kind = kind
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
}

// TestRestoreNewestStatus_SetsMappedState checks that the restore write uses
// the same table as the end write on each forge kind.
func TestRestoreNewestStatus_SetsMappedState(t *testing.T) {
	contexts := map[forge.Kind]string{
		forge.KindGitHub: "continuous-integration/jenkins/branch",
		forge.KindGitea:  giteaHeadContext,
	}
	for kind, states := range pluginResultStates {
		for result, wantState := range states {
			t.Run(string(kind)+" "+result, func(t *testing.T) {
				fx := newFixture(t, "agents-payments")
				fx.kind = kind
				c := pushable()
				c.statuses = []forge.CommitStatus{
					{Context: contexts[kind], State: forge.StateSuccess, TargetURL: statusOlderURL},
				}
				oneWriter(fx, c)
				n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq(result))
				if err != nil || n != 1 {
					t.Fatalf("written %d, err %v; want 1, nil", n, err)
				}
				if got := c.statusWrites[0].State; got != wantState {
					t.Fatalf("state = %q, want %q", got, wantState)
				}
			})
		}
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

// TestEndPendingStatus_ResultLeavesQueuedStatusOfAnotherJenkins checks that
// a Result request leaves the queued status at the job URL of another
// Jenkins.
func TestEndPendingStatus_ResultLeavesQueuedStatusOfAnotherJenkins(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: "https://jenkins.example.org/job/payments/job/app/job/main/display/redirect"},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), resultReq("SUCCESS"))
	if err != nil || n != 0 || len(c.statusWrites) != 0 {
		t.Fatalf("written %d, writes %d, err %v; want none", n, len(c.statusWrites), err)
	}
}

// TestJobURLUnder covers the job URL forms that a queued status carries,
// under the Jenkins location URL of the cell.
func TestJobURLUnder(t *testing.T) {
	const plain = "https://j.example.org/"
	cases := []struct {
		name   string
		target string
		root   string
		job    string
		want   bool
	}{
		{"plain job url", plain + "job/payments/job/app/job/main/", plain, "payments/app/main", true},
		{"job url with no trailing slash", plain + "job/payments/job/app/job/main", plain, "payments/app/main", true},
		{"display redirect", statusJobURL, statusRoot, "payments/app/main", true},
		{"context path prefix", "https://j.example.org/jenkins/job/payments/job/app/job/main/display/redirect", "https://j.example.org/jenkins/", "payments/app/main", true},
		{"encoded branch slash", plain + "job/payments/job/app/job/feature%2Fx/display/redirect", plain, "payments/app/feature%2Fx", true},
		{"doubly encoded branch slash", plain + "job/payments/job/app/job/feature%252Fx/", plain, "payments/app/feature%2Fx", true},
		{"build url", statusBuildURL, statusRoot, "payments/app/main", false},
		{"build url with redirect", plain + "job/payments/job/app/job/main/7/display/redirect", plain, "payments/app/main", false},
		{"parent folder", plain + "job/payments/job/app/", plain, "payments/app/main", false},
		{"another job", plain + "job/payments/job/other/job/main/", plain, "payments/app/main", false},
		{"nested job with the same tail", plain + "job/other/job/payments/job/app/job/main/", plain, "payments/app/main", false},
		{"another host", "https://other.example.org/job/payments/job/app/job/main/", plain, "payments/app/main", false},
		{"another cell", "https://varroa.example.org/headless/core/cells/cell-b/job/payments/job/app/job/main/display/redirect", statusRoot, "payments/app/main", false},
		{"url that does not parse", "https://[::1/job/payments/job/app/job/main/", plain, "payments/app/main", false},
		{"empty target", "", plain, "payments/app/main", false},
		{"empty root", plain + "job/payments/job/app/job/main/", "", "payments/app/main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobURLUnder(tc.target, tc.root, tc.job); got != tc.want {
				t.Fatalf("jobURLUnder(%q, %q) = %v, want %v", tc.target, tc.root, got, tc.want)
			}
		})
	}
}
