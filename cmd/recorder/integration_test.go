package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// requireTestDSN returns the test database DSN, or skips the calling
// test when the environment gives none.
func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("VARROA_TEST_DSN")
	if dsn == "" {
		t.Skipf("VARROA_TEST_DSN is not set; skipping recorder integration test")
	}
	return dsn
}

// resetSchema drops schema "varroa" from dsn's database, so run's own
// migration pass starts clean.
func resetSchema(t *testing.T, dsn string) {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		t.Fatalf("drop schema varroa: %v", err)
	}
}

// startTestNATSServer starts an embedded NATS server with JetStream
// enabled, for this test's empty in-process bus.
func startTestNATSServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestRunEnsuresBuildsStreamWithD3Config starts the recorder's run loop
// against an empty in-process JetStream and the test Postgres, and
// asserts the varroa_builds stream exists with design D3's
// configuration.
func TestRunEnsuresBuildsStreamWithD3Config(t *testing.T) {
	dsn := requireTestDSN(t)
	resetSchema(t, dsn)
	natsServer := startTestNATSServer(t)

	cfg := Config{
		Listen:             "127.0.0.1:0",
		RecordStoreDSN:     dsn,
		BlobStoreURL:       "pgblob:",
		BusURL:             natsServer.ClientURL(),
		JetStreamReplicas:  1,
		RetentionRuns:      365 * 24 * time.Hour,
		RetentionLogs:      90 * 24 * time.Hour,
		RetentionArtifacts: 30 * 24 * time.Hour,
		FinalizeWait:       time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	conn, err := bus.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatalf("connect to test bus: %v", err)
	}
	defer conn.Close()

	var info *nats.StreamInfo
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		info, err = conn.JetStream().StreamInfo(bus.BuildsStreamName)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("varroa_builds stream never appeared: %v", err)
	}

	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != bus.BuildsWildcard {
		t.Errorf("subjects = %v, want [%s]", info.Config.Subjects, bus.BuildsWildcard)
	}
	if info.Config.Retention != nats.WorkQueuePolicy {
		t.Errorf("retention = %v, want WorkQueuePolicy", info.Config.Retention)
	}
	if info.Config.Discard != nats.DiscardNew {
		t.Errorf("discard = %v, want DiscardNew", info.Config.Discard)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned an error during shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not shut down after the context was cancelled")
	}
}

// TestRunLedgerVetoesClaimWithNoPlacementRow starts the recorder's real
// run loop and asserts a varroa.ledger.claim request for a tenant with
// no tenant_placements row is vetoed NOT_READY, through the exact ledger
// composition run wires: NewLedger(pool, NewPlacementFence(pool,
// fenceMetrics)). Before this glue, run wired an allow-all Fence and the
// same claim would have been granted.
func TestRunLedgerVetoesClaimWithNoPlacementRow(t *testing.T) {
	dsn := requireTestDSN(t)
	resetSchema(t, dsn)
	natsServer := startTestNATSServer(t)

	cfg := Config{
		Listen:             "127.0.0.1:0",
		RecordStoreDSN:     dsn,
		BlobStoreURL:       "pgblob:",
		BusURL:             natsServer.ClientURL(),
		JetStreamReplicas:  1,
		RetentionRuns:      365 * 24 * time.Hour,
		RetentionLogs:      90 * 24 * time.Hour,
		RetentionArtifacts: 30 * 24 * time.Hour,
		FinalizeWait:       time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()
	defer func() {
		cancel()
		<-runErr
	}()

	conn, err := bus.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatalf("connect to test bus: %v", err)
	}
	defer conn.Close()

	runID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new run id: %v", err)
	}
	req := recordstore.ClaimRequest{
		IntentKey:    "manual:" + runID.String(),
		RunID:        runID.String(),
		Cluster:      "core",
		Namespace:    "team-a",
		Cell:         "cell-1",
		CellInstance: "pod-1",
		Tenant:       "unannounced-tenant",
		JobPath:      "folder/job",
		Actor:        "tester",
		RequestedAt:  time.Now().UTC(),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal claim request: %v", err)
	}

	var msg *nats.Msg
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg, err = conn.Request(bus.LedgerClaimSubject, payload, 3*time.Second)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("claim request never answered: %v", err)
	}

	var resp recordstore.ClaimResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if resp.Decision != recordstore.DecisionVetoed || resp.ReasonCode != recordstore.ReasonNotReady {
		t.Fatalf("claim response = %+v, want vetoed/NOT_READY", resp)
	}
}
