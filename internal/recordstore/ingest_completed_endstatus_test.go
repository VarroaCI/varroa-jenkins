package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const endStatusRevision = "0123456789abcdef0123456789abcdef01234567"

// TestRunCompleted_SourceGonePublishesEndCommitStatus checks that the owner
// completion of a dead branch job publishes end_commit_status with the result.
func TestRunCompleted_SourceGonePublishesEndCommitStatus(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	received := subscribeEndStatusForTest(t, conn)

	g := NewIngester(pool, conn, nil)
	runID := seedGrantedIntent(t, pool, "manual:source-gone")
	started := newTestEnvelope(t, runID, "manual:source-gone", KindRunStarted, map[string]any{
		"build_number": 7, "scm_revision": endStatusRevision, "scm_url": "https://git.example.org/acme/app",
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:source-gone", KindRunCompleted, map[string]any{
		"result": "FAILURE", "source_gone": true, "seq": 2,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionEndCommitStatus || a.Result != "FAILURE" || a.Reason != "" {
			t.Fatalf("action = %+v", a)
		}
		if a.Repo != "https://git.example.org/acme/app" || a.Revision != endStatusRevision ||
			a.BuildNumber == nil || *a.BuildNumber != 7 || a.Tenant != "acme" ||
			a.JobPath == "" || a.Cluster != testCluster {
			t.Fatalf("action fields = %+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no end_commit_status action")
	}
}

// TestRunCompleted_LiveSourcePublishesNoEndCommitStatus checks that a
// completion of a live branch job leaves the status to the plugin.
func TestRunCompleted_LiveSourcePublishesNoEndCommitStatus(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	received := subscribeEndStatusForTest(t, conn)

	g := NewIngester(pool, conn, nil)
	runID := seedGrantedIntent(t, pool, "manual:source-live")
	started := newTestEnvelope(t, runID, "manual:source-live", KindRunStarted, map[string]any{
		"build_number": 7, "scm_revision": endStatusRevision, "scm_url": "https://git.example.org/acme/app",
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:source-live", KindRunCompleted, map[string]any{
		"result": "SUCCESS", "seq": 2,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	expectNoEndStatus(t, received)
}

// TestRunCompleted_SourceGoneWithoutRepoPublishesNothing checks that a dead
// branch job whose run stored no repository publishes no action.
func TestRunCompleted_SourceGoneWithoutRepoPublishesNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	received := subscribeEndStatusForTest(t, conn)

	g := NewIngester(pool, conn, nil)
	runID := seedGrantedIntent(t, pool, "manual:source-norepo")
	started := newTestEnvelope(t, runID, "manual:source-norepo", KindRunStarted, map[string]any{
		"build_number": 7, "scm_revision": endStatusRevision,
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:source-norepo", KindRunCompleted, map[string]any{
		"result": "FAILURE", "source_gone": true, "seq": 2,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	expectNoEndStatus(t, received)
}

// TestRunCompleted_ForeignSourceGonePublishesNothing checks that only the
// owning cell instance publishes the action.
func TestRunCompleted_ForeignSourceGonePublishesNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	received := subscribeEndStatusForTest(t, conn)

	g := NewIngester(pool, conn, nil)
	runID := seedGrantedIntent(t, pool, "manual:source-foreign")
	started := newTestEnvelope(t, runID, "manual:source-foreign", KindRunStarted, map[string]any{
		"build_number": 7, "scm_revision": endStatusRevision, "scm_url": "https://git.example.org/acme/app",
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:source-foreign", KindRunCompleted, map[string]any{
		"result": "FAILURE", "source_gone": true, "seq": 2,
	})
	completed.CellInstance = "pod-b"
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	expectNoEndStatus(t, received)
}

// subscribeEndStatusForTest collects the core cluster's ledger actions.
func subscribeEndStatusForTest(t *testing.T, conn *bus.Conn) <-chan LedgerAction {
	t.Helper()
	return subscribeLedgerActionsForTest(t, conn)
}

// expectNoEndStatus fails when an end_commit_status action arrives.
func expectNoEndStatus(t *testing.T, received <-chan LedgerAction) {
	t.Helper()
	select {
	case a := <-received:
		if a.Kind == LedgerActionEndCommitStatus {
			t.Fatalf("unexpected end_commit_status: %+v", a)
		}
	case <-time.After(300 * time.Millisecond):
	}
}
