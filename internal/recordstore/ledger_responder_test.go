package recordstore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// startResponderTestServer starts an embedded NATS server for the claim
// responder integration test.
func startResponderTestServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Port:     -1,
		StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// TestClaimResponderIntegration registers the responder on
// bus.LedgerClaimSubject over an in-process NATS server, issues a
// request, and asserts a granted reply.
func TestClaimResponderIntegration(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)

	s := startResponderTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	ledger := NewLedger(pool, nil)
	sub, err := RegisterClaimResponder(conn, ledger, nil, nil)
	if err != nil {
		t.Fatalf("register claim responder: %v", err)
	}
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
	})

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	msg, err := conn.Request(bus.LedgerClaimSubject, body, 5*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	var resp ClaimResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Decision != DecisionGranted {
		t.Fatalf("want decision %q, got %+v", DecisionGranted, resp)
	}
	if resp.RunID != req.RunID {
		t.Fatalf("want run_id %q, got %q", req.RunID, resp.RunID)
	}
}
