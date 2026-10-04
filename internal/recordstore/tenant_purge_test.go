package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// seedTenantPurgeCell is the cell every seeded schedule row names. One
// cell serves every purge test: no test of this file reads the column.
const seedTenantPurgeCell = "cell-a"

// seedTenantPurgeRows applies one job.schedules envelope carrying three
// rows: two for tenant, at oldGen and newGen, and one for a second
// tenant, so a purge test can assert both the generation fence and
// tenant isolation from a single seed call.
func seedTenantPurgeRows(t *testing.T, ing *Ingester, tenant string, oldGen, newGen int64, otherTenant string) {
	t.Helper()
	env := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindJobSchedules,
		Cluster:       testCluster,
		Namespace:     "jenkins",
		Cell:          seedTenantPurgeCell,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
	}
	payload := JobSchedulesPayload{
		Hash: "seed",
		Rows: []JobScheduleRowInput{
			{Tenant: tenant, PlacementGeneration: oldGen, JobPath: tenant + "/old-job", TriggerType: "cron", Spec: "H * * * *"},
			{Tenant: tenant, PlacementGeneration: newGen, JobPath: tenant + "/new-job", TriggerType: "cron", Spec: "H * * * *"},
			{Tenant: otherTenant, PlacementGeneration: oldGen, JobPath: otherTenant + "/job", TriggerType: "cron", Spec: "H * * * *"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env.Payload = raw
	if _, err := ing.Apply(context.Background(), env); err != nil {
		t.Fatalf("seed job schedule rows: %v", err)
	}
}

// countTenantRows counts the varroa.job_schedules rows for the shared
// test cluster "core" and tenant.
func countTenantRows(t *testing.T, db *sql.DB, tenant string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedules WHERE cluster = 'core' AND tenant = $1
	`, tenant).Scan(&n); err != nil {
		t.Fatalf("count job_schedules rows: %v", err)
	}
	return n
}

// TestTenantPurgeResponderIntegration registers the responder on
// bus.TenantPurgeSubject over an in-process NATS server, seeds rows for
// two tenants at two placement generations, and asserts the purge
// deletes only the named tenant's rows at or below the requested
// generation, leaves the newer-generation and other-tenant rows in
// place, and answers a repeat with the same success.
func TestTenantPurgeResponderIntegration(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ing := NewIngester(pool, nil, nil)

	const cluster = "core"
	seedTenantPurgeRows(t, ing, "payments", 3, 5, "billing")

	if got := countTenantRows(t, db, "payments"); got != 2 {
		t.Fatalf("seeded payments rows = %d, want 2", got)
	}
	if got := countTenantRows(t, db, "billing"); got != 1 {
		t.Fatalf("seeded billing rows = %d, want 1", got)
	}

	s := startResponderTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	sub, err := RegisterTenantPurgeResponder(conn, pool, nil)
	if err != nil {
		t.Fatalf("register tenant purge responder: %v", err)
	}
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
	})

	req := TenantPurgeRequest{Cluster: cluster, Tenant: "payments", PlacementGeneration: 4}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	msg, err := conn.Request(bus.TenantPurgeSubject, body, 5*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var resp TenantPurgeResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("want no error, got %q", resp.Error)
	}

	// The old-generation row is gone; the new-generation row for the
	// same tenant survives.
	if got := countTenantRows(t, db, "payments"); got != 1 {
		t.Fatalf("payments rows after purge = %d, want 1", got)
	}
	var remainingJobPath string
	if err := db.QueryRow(`
		SELECT job_path FROM varroa.job_schedules WHERE cluster = $1 AND tenant = 'payments'
	`, cluster).Scan(&remainingJobPath); err != nil {
		t.Fatalf("read remaining payments row: %v", err)
	}
	if remainingJobPath != "payments/new-job" {
		t.Fatalf("remaining payments row = %q, want payments/new-job", remainingJobPath)
	}

	// The other tenant's row is untouched.
	if got := countTenantRows(t, db, "billing"); got != 1 {
		t.Fatalf("billing rows after purge = %d, want 1", got)
	}

	// A repeated request for the same tenant and generation matches no
	// row and still answers success.
	msg2, err := conn.Request(bus.TenantPurgeSubject, body, 5*time.Second)
	if err != nil {
		t.Fatalf("repeat request: %v", err)
	}
	var resp2 TenantPurgeResponse
	if err := json.Unmarshal(msg2.Data, &resp2); err != nil {
		t.Fatalf("unmarshal repeat response: %v", err)
	}
	if resp2.Error != "" {
		t.Fatalf("repeat: want no error, got %q", resp2.Error)
	}
	if got := countTenantRows(t, db, "payments"); got != 1 {
		t.Fatalf("payments rows after repeat purge = %d, want 1", got)
	}
}

// TestHandleTenantPurge_MalformedRequest asserts a request body the
// decoder rejects answers a non-empty error, never a panic or a silent
// success.
func TestHandleTenantPurge_MalformedRequest(t *testing.T) {
	out := handleTenantPurge(slog.Default(), nil, []byte("not json"))
	var resp TenantPurgeResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == "" {
		t.Fatal("want a non-empty error for a malformed request")
	}
}

// TestHandleTenantPurge_MissingFields asserts a request naming no
// cluster or no tenant answers an error without touching the pool, so an
// unscoped delete can never run.
func TestHandleTenantPurge_MissingFields(t *testing.T) {
	cases := []TenantPurgeRequest{
		{Tenant: "payments", PlacementGeneration: 1},
		{Cluster: "core", PlacementGeneration: 1},
	}
	for _, req := range cases {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out := handleTenantPurge(slog.Default(), nil, body)
		var resp TenantPurgeResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Error == "" {
			t.Fatalf("request %+v: want a non-empty error", req)
		}
	}
}

// TestHandleTenantPurge_TeamDeletedWithACellIsRefused asserts a slot move
// that reports a deleted Team answers an error without touching the pool.
// A move that retired the incarnation would refuse the tenant's own next
// announcement.
func TestHandleTenantPurge_TeamDeletedWithACellIsRefused(t *testing.T) {
	body, err := json.Marshal(TenantPurgeRequest{
		Cluster: "core", Tenant: "payments", PlacementGeneration: 1,
		Cell: "cell-a", CellNamespace: "cells", Incarnation: "uid-a", TeamDeleted: true,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var resp TenantPurgeResponse
	if err := json.Unmarshal(handleTenantPurge(slog.Default(), nil, body), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == "" {
		t.Fatal("want a non-empty error for a slot move that reports a deleted Team")
	}
}
