package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAgentNodeNames asserts only an exact nodes/<name>/config.xml path
// names an agent node, and that the answer is sorted and unique.
func TestAgentNodeNames(t *testing.T) {
	t.Parallel()

	files := []runStateFile{
		{RelPath: "nodes/b/config.xml"},
		{RelPath: "nodes/a/config.xml"},
		{RelPath: "nodes/a/config.xml"},
		{RelPath: "jobs/t/jobs/j/nodes/x/config.xml"},
		{RelPath: "nodes/c/d/config.xml"},
		{RelPath: "nodes//config.xml"},
		{RelPath: "jobs/t/jobs/j/builds/7/build.xml"},
	}
	got := agentNodeNames(files)
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agentNodeNames = %v, want %v", got, want)
	}
	if got := agentNodeNames(nil); len(got) != 0 {
		t.Fatalf("agentNodeNames(nil) = %v, want empty", got)
	}
}

// seedRunNodeFiles stores one run's replicated file list, so its
// mesh.files names the given relative paths.
func seedRunNodeFiles(t *testing.T, db *sql.DB, runID uuid.UUID, relPaths ...string) {
	t.Helper()
	files := make([]runStateFile, 0, len(relPaths))
	for _, p := range relPaths {
		files = append(files, runStateFile{RelPath: p, Bytes: 1, Seq: 1, StateGen: 1, Epoch: meshPusherEpoch})
	}
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatalf("marshal run files: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = jsonb_set(COALESCE(payload, '{}'::jsonb), '{mesh}',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('files', $2::jsonb))
		 WHERE run_id = $1
	`, runID, string(raw)); err != nil {
		t.Fatalf("seed run files: %v", err)
	}
}

// seedTenantAgentNamespace sets the agent namespace of the test tenant's
// placement row, which the release action names.
func seedTenantAgentNamespace(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET agent_namespace = $3 WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, "sbsoftware"); err != nil {
		t.Fatalf("seed agent namespace: %v", err)
	}
}

// TestReleaseRunAgentsPublishesNodeNames asserts a run that ended with
// agent nodes publishes one release_agents action that names the tenant,
// its agent namespace, and only the run's own node names.
func TestReleaseRunAgentsPublishesNodeNames(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, runID, "nodes/b/config.xml", "nodes/a/config.xml",
		"jobs/t/jobs/j/builds/7/build.xml", "jobs/t/jobs/j/nodes/x/config.xml")
	fresh := time.Now()
	seedTenantPlacementLiveness(t, db, "pod-a", true, &fresh, &fresh, nil)
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	releaseRunAgents(context.Background(), pool, conn, nil, fetchRunningCandidate(t, pool, runID), nil)

	select {
	case a := <-received:
		if a.Kind != LedgerActionReleaseAgents {
			t.Fatalf("kind = %q, want %q", a.Kind, LedgerActionReleaseAgents)
		}
		if a.Tenant != testTenant || a.AgentNamespace != "sbsoftware" || a.RunID != runID.String() {
			t.Fatalf("action = %+v, want tenant %q in sbsoftware for %s", a, testTenant, runID)
		}
		if want := []string{"a", "b"}; !reflect.DeepEqual(a.AgentPods, want) {
			t.Fatalf("agent pods = %v, want %v", a.AgentPods, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a release_agents action published")
	}
}

// TestReleaseRunAgentsSkipsRunWithNoNodes asserts a run that held no
// agent node, or whose tenant has no agent namespace, publishes nothing.
func TestReleaseRunAgentsSkipsRunWithNoNodes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, runID, "jobs/t/jobs/j/builds/7/build.xml")
	fresh := time.Now()
	seedTenantPlacementLiveness(t, db, "pod-a", true, &fresh, &fresh, nil)
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	releaseRunAgents(context.Background(), pool, conn, nil, fetchRunningCandidate(t, pool, runID), nil)

	select {
	case a := <-received:
		t.Fatalf("want no action for a run with no agent node, got %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestSweepInterruptedRunsReleasesNoAgentsForHeartbeatLost asserts a
// heartbeat-lost mark, which is provisional, publishes no release_agents
// action: the owner can still resume through the same agent pods.
func TestSweepInterruptedRunsReleasesNoAgentsForHeartbeatLost(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{}`)
	seedRunNodeFiles(t, db, runID, "nodes/a/config.xml")
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := subscribeClusterActions(t, conn)

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, &prefixRecordingStore{},
		DefaultSweepMaxPending, time.Now(), testGrace, "", nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}
	select {
	case a := <-received:
		if a.Kind == LedgerActionReleaseAgents {
			t.Fatalf("heartbeat-lost published release_agents: %+v", a)
		}
	case <-time.After(500 * time.Millisecond):
	}
}

// TestReleaseRunAgentsKeepsNodeHeldByAnotherRunningRun asserts a node name
// that another RUNNING run of the tenant also lists is not released, because
// a later run may hold the pod with that name.
func TestReleaseRunAgentsKeepsNodeHeldByAnotherRunningRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, runID, "nodes/a/config.xml", "nodes/b/config.xml")
	otherID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, otherID, "nodes/a/config.xml")
	fresh := time.Now()
	seedTenantPlacementLiveness(t, db, "pod-a", true, &fresh, &fresh, nil)
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	releaseRunAgents(context.Background(), pool, conn, nil, fetchRunningCandidate(t, pool, runID), nil)

	select {
	case a := <-received:
		if want := []string{"b"}; !reflect.DeepEqual(a.AgentPods, want) {
			t.Fatalf("agent pods = %v, want %v", a.AgentPods, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a release_agents action published")
	}
}
