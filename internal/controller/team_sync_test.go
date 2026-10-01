package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// startTestSyncBus follows the embedded-NATS pattern used across the
// package (see cell_readiness_test.go's TestCellRolledActivityEvent),
// gated the same way.
func startTestSyncBus(t *testing.T) *bus.Conn {
	t.Helper()
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}

	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// TestNewNATSSyncAnnouncer_RequestShape asserts task 4.1: the "announce"
// request carries every field design D6 names, over bus.TenantSyncSubject.
func TestNewNATSSyncAnnouncer_RequestShape(t *testing.T) {
	conn := startTestSyncBus(t)

	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true, InterruptedMarked: 2})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	announcer := NewNATSSyncAnnouncer(conn)
	aliveAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lastSeen := aliveAt.Add(-time.Minute)
	result, err := announcer(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "payments", Namespace: "cells-ns", Cell: "cell-a",
		Generation: 3, CellInstance: "uid-2",
		Policy: cells.SyncPolicy{
			RehydrateBuilds: 5, RehydrateDeadline: 30 * time.Minute, InterruptedRetry: "automatic",
			Concurrency: "cancelInProgress", CronBackstop: true, CronCatchUp: "one", CronGrace: 5 * time.Minute,
			Exports: []v1alpha1.TeamExport{{Job: "release/*", Callers: []string{"payments", "search"}}},
		},
		RehydratedInstance: "uid-2", PendingJobs: []string{"payments/api/main"},
		CellAlive: true, CellAliveAt: &aliveAt, CellLastSeenAt: &lastSeen,
		DeadInstances:  []cells.DeadInstance{{Instance: "uid-1", LastSeenAt: lastSeen}},
		AgentNamespace: "payments-agents",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !result.OK || result.InterruptedMarked != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}

	if got.Kind != recordstore.TenantSyncKindAnnounce {
		t.Errorf("Kind = %q, want %q", got.Kind, recordstore.TenantSyncKindAnnounce)
	}
	if got.Cluster != "core" || got.Tenant != "payments" || got.Namespace != "cells-ns" || got.Cell != "cell-a" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Generation != 3 || got.CellInstance != "uid-2" || got.RehydratedInstance != "uid-2" {
		t.Errorf("generation/instance fields wrong: %+v", got)
	}
	if len(got.PendingJobs) != 1 || got.PendingJobs[0] != "payments/api/main" {
		t.Errorf("PendingJobs = %v", got.PendingJobs)
	}
	if !got.CellAlive || got.CellAliveAt == nil || got.CellLastSeenAt == nil {
		t.Errorf("presence fields wrong: %+v", got)
	}
	if len(got.DeadInstances) != 1 || got.DeadInstances[0].Instance != "uid-1" {
		t.Errorf("DeadInstances = %v", got.DeadInstances)
	}
	if got.AgentNamespace != "payments-agents" {
		t.Errorf("AgentNamespace = %q, want payments-agents", got.AgentNamespace)
	}

	var policy syncPolicyWire
	if err := json.Unmarshal(got.Policy, &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if policy.RehydrateBuilds != 5 || policy.InterruptedRetry != "automatic" || policy.Concurrency != "cancelInProgress" ||
		!policy.CronBackstop || policy.CronCatchUp != "one" || policy.RehydrateDeadline != "30m0s" || policy.CronGrace != "5m0s" {
		t.Errorf("policy wire shape wrong: %+v", policy)
	}
	if len(policy.Exports) != 1 || policy.Exports[0].Job != "release/*" ||
		len(policy.Exports[0].Callers) != 2 || policy.Exports[0].Callers[0] != "payments" || policy.Exports[0].Callers[1] != "search" {
		t.Errorf("policy.Exports = %+v, want one release/* export for payments and search", policy.Exports)
	}
}

// TestNATSSyncAnnouncerMapsAStaleReply asserts a stale refusal reaches the
// placement pass as a failed announce, so the pass caches nothing.
func TestNATSSyncAnnouncerMapsAStaleReply(t *testing.T) {
	conn := startTestSyncBus(t)

	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func([]byte) []byte {
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Stale: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	result, err := NewNATSSyncAnnouncer(conn)(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "payments", Cell: "cell-a", Generation: 3, CellInstance: "uid-1",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if result.OK || !result.Stale {
		t.Fatalf("want a stale, failed result, got %+v", result)
	}
}

// TestNewNATSSyncRecordsRequester_RequestShape asserts task 4.1's other
// kind: a "records" request and its reply.
func TestNewNATSSyncRecordsRequester_RequestShape(t *testing.T) {
	conn := startTestSyncBus(t)

	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{
			Jobs: []recordstore.TenantSyncJobRecords{
				{JobPath: "payments/api/main", NextBuildNumber: 4, Records: []recordstore.TenantSyncRecord{
					{BuildNumber: 3, Result: "SUCCESS"},
				}},
			},
		})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	requester := NewNATSSyncRecordsRequester(conn)
	result, err := requester(context.Background(), cells.RecordsRequest{
		Cluster: "core", Tenant: "payments", JobPaths: []string{"payments/api/main"}, PerJob: 10,
	})
	if err != nil {
		t.Fatalf("records: %v", err)
	}

	if got.Kind != recordstore.TenantSyncKindRecords {
		t.Errorf("Kind = %q, want %q", got.Kind, recordstore.TenantSyncKindRecords)
	}
	if got.Cluster != "core" || got.Tenant != "payments" || got.PerJob != 10 {
		t.Errorf("request fields wrong: %+v", got)
	}
	if len(got.JobPaths) != 1 || got.JobPaths[0] != "payments/api/main" {
		t.Errorf("JobPaths = %v", got.JobPaths)
	}

	if len(result.Jobs) != 1 || result.Jobs[0].JobPath != "payments/api/main" || result.Jobs[0].NextBuildNumber != 4 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Jobs[0].Records) != 1 || result.Jobs[0].Records[0].BuildNumber != 3 || result.Jobs[0].Records[0].Result != "SUCCESS" {
		t.Errorf("unexpected records: %+v", result.Jobs[0].Records)
	}
}

// TestLoggedAnnounceLogsAStaleRefusal asserts the Team reconciler's
// announcer logs a stale refusal and passes the result through unchanged.
func TestLoggedAnnounceLogsAStaleRefusal(t *testing.T) {
	var buf bytes.Buffer
	r := &TeamReconciler{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	if r.loggedAnnounce() != nil {
		t.Fatal("a nil announcer must stay nil")
	}

	r.SetSyncAnnouncer(func(context.Context, cells.AnnounceRequest) (cells.AnnounceResult, error) {
		return cells.AnnounceResult{Stale: true}, nil
	})
	result, err := r.loggedAnnounce()(context.Background(), cells.AnnounceRequest{Tenant: "payments", Generation: 3})
	if err != nil || result.OK || !result.Stale {
		t.Fatalf("want the stale result passed through, got %+v, %v", result, err)
	}
	if !strings.Contains(buf.String(), "recorder refused a stale announce") || !strings.Contains(buf.String(), "team=payments") {
		t.Fatalf("no stale refusal log line: %q", buf.String())
	}

	buf.Reset()
	r.SetSyncAnnouncer(func(context.Context, cells.AnnounceRequest) (cells.AnnounceResult, error) {
		return cells.AnnounceResult{OK: true}, nil
	})
	if _, err := r.loggedAnnounce()(context.Background(), cells.AnnounceRequest{Tenant: "payments"}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("an accepted announce logged: %q", buf.String())
	}
}
