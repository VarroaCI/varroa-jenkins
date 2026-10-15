package mite

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// startBuildEventTestNATS starts an in-process JetStream server for the
// sync publish + STORED integration test. It skips the test, rather than
// failing it, when the embedded server cannot start — the mapping,
// worker, ladder, and ack unit tests already cover this path without a
// live NATS.
func startBuildEventTestNATS(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Skipf("in-process NATS server unavailable: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Skip("in-process NATS server did not become ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestBuildEventIntegration_SyncPublishStored exercises the real path
// end to end against a live JetStream: BusHandler.OnBuildEvent enqueues,
// the worker publishes to varroa_builds with a JetStream ack, and the
// mite receives one EventAck STORED only after that ack.
func TestBuildEventIntegration_SyncPublishStored(t *testing.T) {
	ns := startBuildEventTestNATS(t)

	busConn, err := bus.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	if err := busConn.EnsureStream(bus.BuildsStreamConfig(bus.BuildsStreamName)); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}

	var (
		receivedMu sync.Mutex
		received   []ingestEnvelope
	)
	sub, err := busConn.NATSConn().Subscribe(bus.BuildsWildcard, func(msg *nats.Msg) {
		var env ingestEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			return
		}
		receivedMu.Lock()
		received = append(received, env)
		receivedMu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe builds: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	handler := &BusHandler{
		metrics:        noopGatewayMetrics(),
		cluster:        bus.DefaultCluster,
		conn:           busConn,
		publisher:      busConn,
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: defaultCellTenantResolver{},
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	handler.RecordCellInstance("ctrl1", "team-a", "uid-1")

	sender := &fakeSender{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler.startBuildEventWorker(ctx, "team-a", "ctrl1", 1, sender.send)

	evt := &mitev1.BuildEvent{
		Phase:           phaseStarted,
		RunId:           "run-int-1",
		IntentKey:       "scm:team-a/build:abc",
		JobPath:         "team-a/build",
		BuildNumber:     7,
		Seq:             1,
		EmittedAtMillis: time.Now().UnixMilli(),
	}
	if err := handler.OnBuildEvent("ctrl1", "team-a", 1, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitFor(t, func() bool { return sender.count() == 1 }, 5*time.Second, "no ack received")
	ack := sender.first().GetEventAck()
	if ack == nil || ack.Status != eventAckStored || ack.RunId != "run-int-1" {
		t.Fatalf("ack = %+v, want STORED for run-int-1", ack)
	}

	waitFor(t, func() bool {
		receivedMu.Lock()
		defer receivedMu.Unlock()
		return len(received) == 1
	}, 5*time.Second, "envelope never reached the builds stream")
	receivedMu.Lock()
	env := received[0]
	receivedMu.Unlock()
	if env.Kind != kindRunStarted || env.RunID != "run-int-1" || env.CellInstance != "uid-1" ||
		env.Namespace != "team-a" || env.Cell != "ctrl1" {
		t.Fatalf("envelope = %+v, want run.started for run-int-1 with cell_instance uid-1", env)
	}
}
