package recordstore

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestReleasableDeadInstances proves the entry selector keeps only a
// pod-read confirmation that is past the grace and inside the release
// window.
func TestReleasableDeadInstances(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	grace := 30 * time.Second
	at := func(d time.Duration) *time.Time {
		v := now.Add(-d)
		return &v
	}
	cases := map[string]struct {
		entry TenantSyncDeadInstance
		want  bool
	}{
		"no confirmation":           {TenantSyncDeadInstance{Instance: "pod-old"}, false},
		"inside the grace":          {TenantSyncDeadInstance{Instance: "pod-old", ConfirmedGoneAt: at(10 * time.Second)}, false},
		"confirmed two minutes ago": {TenantSyncDeadInstance{Instance: "pod-old", ConfirmedGoneAt: at(2 * time.Minute)}, true},
		"past the release window":   {TenantSyncDeadInstance{Instance: "pod-old", ConfirmedGoneAt: at(grace + 6*time.Minute)}, false},
		"empty instance":            {TenantSyncDeadInstance{ConfirmedGoneAt: at(2 * time.Minute)}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := releasableDeadInstances([]TenantSyncDeadInstance{c.entry}, now, grace)
			if (len(got) == 1) != c.want {
				t.Fatalf("releasable = %v, want %v", got, c.want)
			}
		})
	}
}

// TestReleaseGoneInstanceAgentsPublishesKeepSet proves one confirmed dead
// instance publishes one release_instance_agents action that names only the
// nodes that a RUNNING run still holds.
func TestReleaseGoneInstanceAgentsPublishesKeepSet(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, runID, "nodes/a/config.xml")
	endedID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, endedID, "nodes/b/config.xml")
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED',
		payload = payload || '{"interrupted_reason":"cell-lost"}' WHERE run_id = $1`, endedID); err != nil {
		t.Fatalf("end second run: %v", err)
	}
	now := time.Now()
	confirmed := now.Add(-2 * time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-new", true, &now, &now, []TenantSyncDeadInstance{{
		Instance: "pod-old", LastSeenAt: now.Add(-3 * time.Minute), ConfirmedGoneAt: &confirmed,
		Cell: "cell-a", Namespace: "cells-ns",
	}})
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	n, err := ReleaseGoneInstanceAgents(context.Background(), pool, conn, nil, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("ReleaseGoneInstanceAgents: %v", err)
	}
	if n != 1 {
		t.Fatalf("published = %d, want 1", n)
	}
	select {
	case a := <-received:
		if a.Kind != LedgerActionReleaseInstanceAgents {
			t.Fatalf("kind = %q, want %q", a.Kind, LedgerActionReleaseInstanceAgents)
		}
		if a.Instance != "pod-old" || a.Tenant != testTenant || a.AgentNamespace != "sbsoftware" {
			t.Fatalf("action = %+v, want instance pod-old for tenant %q in sbsoftware", a, testTenant)
		}
		if want := []string{"a"}; !reflect.DeepEqual(a.KeepPods, want) {
			t.Fatalf("keep pods = %v, want %v", a.KeepPods, want)
		}
		if a.RefreshedBefore == nil || a.RefreshedBefore.UnixMilli() != confirmed.UnixMilli() {
			t.Fatalf("refreshed before = %v, want %v", a.RefreshedBefore, confirmed)
		}
		if a.Namespace != "cells-ns" || a.Controller != "cell-a" {
			t.Fatalf("namespace/controller = %q/%q, want cells-ns/cell-a", a.Namespace, a.Controller)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a release_instance_agents action published")
	}
}

// TestReleaseGoneInstanceAgentsSkipsUnconfirmedAndOld proves an entry with
// no confirmation, and one confirmed past the release window, publish
// nothing.
func TestReleaseGoneInstanceAgentsSkipsUnconfirmedAndOld(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-new", true, &now, &now, []TenantSyncDeadInstance{
		{Instance: "pod-unconfirmed", LastSeenAt: now.Add(-3 * time.Minute), Cell: "cell-a", Namespace: "cells-ns"},
		{Instance: "pod-old", LastSeenAt: now.Add(-11 * time.Minute), ConfirmedGoneAt: &old, Cell: "cell-a", Namespace: "cells-ns"},
	})
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	n, err := ReleaseGoneInstanceAgents(context.Background(), pool, conn, nil, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("ReleaseGoneInstanceAgents: %v", err)
	}
	if n != 0 {
		t.Fatalf("published = %d, want 0", n)
	}
	select {
	case a := <-received:
		t.Fatalf("unexpected action %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestReleaseGoneInstanceAgentsKeepsHeartbeatLostNodes proves a node that a
// heartbeat-lost run stores stays in the keep set, because a resume can
// still adopt it.
func TestReleaseGoneInstanceAgentsKeepsHeartbeatLostNodes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	lostID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedRunNodeFiles(t, db, lostID, "nodes/c/config.xml")
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED',
		payload = payload || '{"interrupted_reason":"heartbeat-lost"}' WHERE run_id = $1`, lostID); err != nil {
		t.Fatalf("mark heartbeat-lost: %v", err)
	}
	now := time.Now()
	confirmed := now.Add(-2 * time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-new", true, &now, &now, []TenantSyncDeadInstance{{
		Instance: "pod-old", LastSeenAt: now.Add(-3 * time.Minute), ConfirmedGoneAt: &confirmed,
		Cell: "cell-a", Namespace: "cells-ns",
	}})
	seedTenantAgentNamespace(t, db)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeClusterActions(t, conn)

	if _, err := ReleaseGoneInstanceAgents(context.Background(), pool, conn, nil, time.Now(), testGrace, nil); err != nil {
		t.Fatalf("ReleaseGoneInstanceAgents: %v", err)
	}
	select {
	case a := <-received:
		if want := []string{"c"}; !reflect.DeepEqual(a.KeepPods, want) {
			t.Fatalf("keep pods = %v, want %v", a.KeepPods, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a release_instance_agents action published")
	}
}
