package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// subscribeLedgerActionsForTest collects the ledger actions of the core
// cluster on conn, in the order the subscriber receives them.
func subscribeLedgerActionsForTest(t *testing.T, conn *bus.Conn) <-chan LedgerAction {
	t.Helper()
	received := make(chan LedgerAction, 8)
	sub, err := conn.Subscribe(bus.LedgerActionSubject("core"), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()
	return received
}

// sourcedCandidate is a run that recorded a commit and a repository, with a
// build number, so the end-commit-status publish applies to it.
func sourcedCandidate(runID uuid.UUID) runInterruptCandidate {
	bn := int64(7)
	return runInterruptCandidate{
		runID: runID, cluster: "core", namespace: "team-a", cell: "cell-1", jobPath: "folder/job",
		tenant: "payments", actor: "alice", status: StatusRunning, buildNumber: &bn,
		scmRevision: "0123456789abcdef0123456789abcdef01234567",
		scmURL:      "https://git.example.org/team/app",
	}
}

// TestAfterMarkPublishesEndCommitStatusBeforeRetry checks that the end of the
// commit status comes first, so the old build's status is ended before the
// automatic retry starts its own build.
func TestAfterMarkPublishesEndCommitStatusBeforeRetry(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	received := subscribeLedgerActionsForTest(t, conn)
	run := sourcedCandidate(uuid.Must(uuid.NewV7()))

	afterMark(context.Background(), conn, JobHome{}, nil, nil, run, ReasonCellLost, interruptedRetryAutomatic, nil)

	var kinds []string
	for len(kinds) < 2 {
		select {
		case a := <-received:
			kinds = append(kinds, a.Kind)
			if a.Kind == LedgerActionEndCommitStatus {
				if a.Repo != run.scmURL || a.Revision != run.scmRevision || a.Tenant != "payments" ||
					a.Reason != ReasonCellLost || a.BuildNumber == nil || *a.BuildNumber != 7 ||
					a.RunID != run.runID.String() || a.JobPath != run.jobPath || a.Cluster != "core" {
					t.Fatalf("end_commit_status = %+v", a)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; kinds so far %v", kinds)
		}
	}
	if kinds[0] != LedgerActionEndCommitStatus || kinds[1] != LedgerActionRetryRun {
		t.Fatalf("kinds = %v, want [end_commit_status retry_run]", kinds)
	}
}

// expectNoLedgerAction fails when an action arrives within a short window.
func expectNoLedgerAction(t *testing.T, received <-chan LedgerAction) {
	t.Helper()
	select {
	case a := <-received:
		t.Fatalf("want no action, got %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAfterMarkPublishesNoEndCommitStatusForHeartbeatLost checks that a
// provisional mark keeps the status pending, because the owner may still
// finish the build and write its own status.
func TestAfterMarkPublishesNoEndCommitStatusForHeartbeatLost(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	received := subscribeLedgerActionsForTest(t, conn)
	run := sourcedCandidate(uuid.Must(uuid.NewV7()))

	afterMark(context.Background(), conn, JobHome{}, nil, nil, run, ReasonHeartbeatLost, interruptedRetryAutomatic, nil)
	expectNoLedgerAction(t, received)
}

// TestAfterMarkPublishesNoEndCommitStatusWithoutRevision checks that a run
// with no recorded commit publishes no end action.
func TestAfterMarkPublishesNoEndCommitStatusWithoutRevision(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	received := subscribeLedgerActionsForTest(t, conn)
	run := sourcedCandidate(uuid.Must(uuid.NewV7()))
	run.scmRevision = ""

	afterMark(context.Background(), conn, JobHome{}, nil, nil, run, ReasonCellLost, "", nil)
	expectNoLedgerAction(t, received)
}

// TestAfterMarkPublishesNoEndCommitStatusForQueued checks that a marked queued
// run publishes no end action, because no build ever started.
func TestAfterMarkPublishesNoEndCommitStatusForQueued(t *testing.T) {
	t.Parallel()

	conn := newTestActionConn(t)
	received := subscribeLedgerActionsForTest(t, conn)
	run := sourcedCandidate(uuid.Must(uuid.NewV7()))
	run.status = StatusQueued

	afterMark(context.Background(), conn, JobHome{}, nil, nil, run, ReasonCellLost, "", nil)
	expectNoLedgerAction(t, received)
}

// TestSelectCandidatesCarriesSourceFacts checks that the candidate read takes
// the commit from the row and the repository from run.source, or else from
// the run.started payload.
func TestSelectCandidatesCarriesSourceFacts(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	withSource := uuid.Must(uuid.NewV7())
	startedOnly := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
		                         status, scm_revision, payload)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'RUNNING', 'abc123',
		        '{"run.source": {"scm_url": "https://git.example.org/team/app"}}'::jsonb)
	`, withSource, "manual:"+withSource.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed run with run.source: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
		                         status, payload)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'RUNNING',
		        '{"run.started": {"payload": {"scm_url": "https://git.example.org/team/other"}}}'::jsonb)
	`, startedOnly, "manual:"+startedOnly.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed run with run.started only: %v", err)
	}

	cands, err := selectCandidates(context.Background(), pool)
	if err != nil {
		t.Fatalf("select candidates: %v", err)
	}
	got := map[uuid.UUID]runInterruptCandidate{}
	for _, c := range cands {
		got[c.runID] = c
	}
	if c := got[withSource]; c.scmRevision != "abc123" || c.scmURL != "https://git.example.org/team/app" {
		t.Fatalf("with run.source: revision %q url %q", c.scmRevision, c.scmURL)
	}
	if c := got[startedOnly]; c.scmRevision != "" || c.scmURL != "https://git.example.org/team/other" {
		t.Fatalf("with run.started only: revision %q url %q", c.scmRevision, c.scmURL)
	}
}
