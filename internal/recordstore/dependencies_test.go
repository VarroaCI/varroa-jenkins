package recordstore

import (
	"context"
	"database/sql"
	"testing"
)

// seedUpstreamTrigger inserts one varroa.job_schedules row carrying a
// varroa-upstream trigger, so a dependency-edge test can build a
// producer/subscriber fixture directly, without the ingest pipeline.
func seedUpstreamTrigger(t *testing.T, db *sql.DB, cluster, tenant, jobPath, upstreamTenant, upstreamJob, upstreamResult string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload,
			 upstream_tenant, upstream_job, upstream_result)
		VALUES ($1, 'jenkins', 'cell-a', $2, $3, 'varroa-upstream', now(), '{}', $4, $5, $6)
	`, cluster, tenant, jobPath, upstreamTenant, upstreamJob, upstreamResult); err != nil {
		t.Fatalf("seed upstream trigger row: %v", err)
	}
}

// TestJobDependencies_UpstreamAndDownstream asserts the same declared
// edge appears as an upstream edge on the subscriber's job and a
// downstream edge on the producer's job, both granted when the
// producer's exports cover the subscriber.
func TestJobDependencies_UpstreamAndDownstream(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, testCluster, "alpha", "jenkins", "cell-alpha", 1,
		`{"exports":[{"job":"release","callers":["beta"]}]}`)
	seedGrantPlacement(t, db, testCluster, "beta", "jenkins", "cell-beta", 1, "{}")
	seedUpstreamTrigger(t, db, testCluster, "beta", "beta/rebuild", "alpha", "release", "SUCCESS")

	subscriber, err := reader.JobDependencies(ctx, testCluster, "beta", "beta/rebuild")
	if err != nil {
		t.Fatalf("JobDependencies(beta/rebuild): %v", err)
	}
	if len(subscriber.Upstream) != 1 {
		t.Fatalf("subscriber upstream = %+v, want one edge", subscriber.Upstream)
	}
	want := DependencyEdge{Tenant: "alpha", Job: "release", Result: "SUCCESS", Granted: true}
	if subscriber.Upstream[0] != want {
		t.Fatalf("subscriber upstream[0] = %+v, want %+v", subscriber.Upstream[0], want)
	}
	if len(subscriber.Downstream) != 0 {
		t.Fatalf("subscriber downstream = %+v, want none", subscriber.Downstream)
	}

	producer, err := reader.JobDependencies(ctx, testCluster, "alpha", "alpha/release")
	if err != nil {
		t.Fatalf("JobDependencies(alpha/release): %v", err)
	}
	if len(producer.Upstream) != 0 {
		t.Fatalf("producer upstream = %+v, want none", producer.Upstream)
	}
	if len(producer.Downstream) != 1 {
		t.Fatalf("producer downstream = %+v, want one edge", producer.Downstream)
	}
	wantDownstream := DependencyEdge{Tenant: "beta", Job: "rebuild", Result: "SUCCESS", Granted: true}
	if producer.Downstream[0] != wantDownstream {
		t.Fatalf("producer downstream[0] = %+v, want %+v", producer.Downstream[0], wantDownstream)
	}
}

// TestJobDependencies_DownstreamSubscriberInAnotherCluster asserts a
// downstream edge whose subscriber is placed in a different cluster
// than the producer still resolves granted: the grant check must
// resolve the subscriber's own placement, not the producer's cluster.
func TestJobDependencies_DownstreamSubscriberInAnotherCluster(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()
	const subscriberCluster = "other-cluster"

	seedGrantPlacement(t, db, testCluster, "alpha", "jenkins", "cell-alpha", 1,
		`{"exports":[{"job":"release","callers":["beta"]}]}`)
	seedGrantPlacement(t, db, subscriberCluster, "beta", "jenkins", "cell-beta", 1, "{}")
	seedUpstreamTrigger(t, db, subscriberCluster, "beta", "beta/rebuild", "alpha", "release", "SUCCESS")

	producer, err := reader.JobDependencies(ctx, testCluster, "alpha", "alpha/release")
	if err != nil {
		t.Fatalf("JobDependencies(alpha/release): %v", err)
	}
	if len(producer.Downstream) != 1 {
		t.Fatalf("producer downstream = %+v, want one edge", producer.Downstream)
	}
	want := DependencyEdge{Tenant: "beta", Job: "rebuild", Result: "SUCCESS", Granted: true}
	if producer.Downstream[0] != want {
		t.Fatalf("producer downstream[0] = %+v, want %+v (a subscriber in another cluster must still resolve granted)",
			producer.Downstream[0], want)
	}
}

// TestJobDependencies_UngrantedEdgeStaysVisible asserts a declared edge
// whose producer grants nobody still appears, with Granted false, rather
// than being dropped from the list.
func TestJobDependencies_UngrantedEdgeStaysVisible(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, testCluster, "alpha", "jenkins", "cell-alpha", 1, "{}")
	seedGrantPlacement(t, db, testCluster, "beta", "jenkins", "cell-beta", 1, "{}")
	seedUpstreamTrigger(t, db, testCluster, "beta", "beta/rebuild", "alpha", "release", "SUCCESS")

	subscriber, err := reader.JobDependencies(ctx, testCluster, "beta", "beta/rebuild")
	if err != nil {
		t.Fatalf("JobDependencies(beta/rebuild): %v", err)
	}
	if len(subscriber.Upstream) != 1 {
		t.Fatalf("subscriber upstream = %+v, want one edge", subscriber.Upstream)
	}
	want := DependencyEdge{Tenant: "alpha", Job: "release", Result: "SUCCESS", Granted: false}
	if subscriber.Upstream[0] != want {
		t.Fatalf("subscriber upstream[0] = %+v, want %+v", subscriber.Upstream[0], want)
	}

	producer, err := reader.JobDependencies(ctx, testCluster, "alpha", "alpha/release")
	if err != nil {
		t.Fatalf("JobDependencies(alpha/release): %v", err)
	}
	if len(producer.Downstream) != 1 || producer.Downstream[0].Granted {
		t.Fatalf("producer downstream = %+v, want one ungranted edge", producer.Downstream)
	}
}

// TestJobDependencies_EmptyForNoEdge asserts a job with no
// varroa-upstream trigger and no subscriber reports two empty, non-nil
// arrays.
func TestJobDependencies_EmptyForNoEdge(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, testCluster, "acme", "jenkins", "cell-a", 1, "{}")

	deps, err := reader.JobDependencies(ctx, testCluster, "acme", "acme/lonely")
	if err != nil {
		t.Fatalf("JobDependencies(acme/lonely): %v", err)
	}
	if deps.Upstream == nil || len(deps.Upstream) != 0 {
		t.Fatalf("upstream = %+v, want an empty, non-nil slice", deps.Upstream)
	}
	if deps.Downstream == nil || len(deps.Downstream) != 0 {
		t.Fatalf("downstream = %+v, want an empty, non-nil slice", deps.Downstream)
	}
}
