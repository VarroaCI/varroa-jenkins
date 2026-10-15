package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestRunRegistersTenantSyncResponder starts the recorder's run loop
// against an empty in-process JetStream and the test Postgres, and
// asserts a request on bus.TenantSyncSubject gets an answer, proving the
// tenant sync responder's subscription exists once run has started.
func TestRunRegistersTenantSyncResponder(t *testing.T) {
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

	req := recordstore.TenantSyncRequest{
		Kind: recordstore.TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 0, CellInstance: "pod-a",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var msg *nats.Msg
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		m, reqErr := conn.Request(bus.TenantSyncSubject, body, time.Second)
		if reqErr == nil {
			msg = m
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if msg == nil {
		cancel()
		t.Fatal("tenant sync responder never answered")
	}

	var resp recordstore.TenantSyncResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("want ok, got %+v", resp)
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
