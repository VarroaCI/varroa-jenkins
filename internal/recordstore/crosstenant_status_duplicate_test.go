package recordstore

import (
	"context"
	"testing"
)

// TestCrossTenantRunStateMapsEachStatus asserts the state each stored
// runs status answers. A DUPLICATE row answers the terminal state
// duplicate, with no result and with the result its owner completion
// stored.
func TestCrossTenantRunStateMapsEachStatus(t *testing.T) {
	t.Parallel()

	aborted := "ABORTED"
	interrupted := "INTERRUPTED"
	success := "SUCCESS"
	cases := []struct {
		name string
		rec  crossTenantRunRecord
		want string
	}{
		{"no runs row", crossTenantRunRecord{}, CrossTenantStateQueued},
		{"queued", crossTenantRunRecord{hasRuns: true, status: StatusQueued}, CrossTenantStateQueued},
		{"running", crossTenantRunRecord{hasRuns: true, status: StatusRunning}, CrossTenantStateRunning},
		{"completed", crossTenantRunRecord{hasRuns: true, status: StatusCompleted, result: &success},
			CrossTenantStateCompleted},
		{"interrupted", crossTenantRunRecord{hasRuns: true, status: StatusCompleted, result: &interrupted},
			CrossTenantStateInterrupted},
		{"duplicate before its completion", crossTenantRunRecord{hasRuns: true, status: StatusDuplicate},
			CrossTenantStateDuplicate},
		{"duplicate after its completion", crossTenantRunRecord{hasRuns: true, status: StatusDuplicate, result: &aborted},
			CrossTenantStateDuplicate},
	}
	for _, tc := range cases {
		if got := crossTenantRunState(tc.rec); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCrossTenantStatus_DuplicateRunAnswersDuplicate asserts that a
// granted caller that polls a run the duplicate backstop marked reads
// the terminal state duplicate, by run identifier and by intent key. The
// state holds before the run's owner completion and after it, and the
// reply carries the result that completion stored.
func TestCrossTenantStatus_DuplicateRunAnswersDuplicate(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	g := NewIngester(deps.Pool, nil, nil)
	p := seedDuplicatePair(t, deps.Pool, g, "dup-crosstenant")
	seedGrantPlacement(t, db, testCluster, "alpha", "ns-alpha", "cell-alpha", 1, "{}")
	seedGrantPlacement(t, db, testCluster, testTenant, testNamespace, "cell-a", 1,
		`{"exports":[{"job":"app","callers":["alpha"]}]}`)

	poll := func(req CrossTenantRequest) CrossTenantResponse {
		t.Helper()
		req.Cluster = testCluster
		req.Namespace = "ns-alpha"
		req.Cell = "cell-alpha"
		req.CallerTenant = "alpha"
		req.PlacementGeneration = 1
		resp, err := handleCrossTenantStatus(context.Background(), deps, req)
		if err != nil {
			t.Fatalf("handleCrossTenantStatus: %v", err)
		}
		if resp.Code != "" || resp.ReasonCode != "" {
			t.Fatalf("got code %q reason_code %q (%s%s), want an allowed reply",
				resp.Code, resp.ReasonCode, resp.Error, resp.Message)
		}
		if resp.RunID != p.duplicate.String() {
			t.Fatalf("run_id: got %q, want %q", resp.RunID, p.duplicate)
		}
		if resp.State != CrossTenantStateDuplicate {
			t.Fatalf("state: got %q, want %q", resp.State, CrossTenantStateDuplicate)
		}
		return resp
	}
	requests := []CrossTenantRequest{{RunID: p.duplicate.String()}, {IntentKey: p.duplicateKey}}

	for _, req := range requests {
		if resp := poll(req); resp.Result != nil {
			t.Fatalf("result before the completion: got %q, want none", *resp.Result)
		}
	}

	completeTestRun(t, g, p.duplicate, p.duplicateKey, "ABORTED", 50, nil)
	for _, req := range requests {
		if resp := poll(req); resp.Result == nil || *resp.Result != "ABORTED" {
			t.Fatalf("result after the completion: got %v, want ABORTED", resp.Result)
		}
	}

	resp, err := handleCrossTenantStatus(context.Background(), deps, CrossTenantRequest{
		Cluster: testCluster, Namespace: "ns-alpha", Cell: "cell-alpha", CallerTenant: "alpha",
		PlacementGeneration: 1, RunID: p.older.String(),
	})
	if err != nil {
		t.Fatalf("handleCrossTenantStatus older run: %v", err)
	}
	if resp.State != CrossTenantStateRunning {
		t.Fatalf("older run state: got %q, want %q", resp.State, CrossTenantStateRunning)
	}
}
