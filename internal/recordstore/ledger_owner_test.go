package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestOwnerResponderIntegration registers the responder on
// bus.LedgerOwnerSubject over an in-process NATS server, drives a known
// run, an unknown run, and a timeout, and asserts each answer.
func TestOwnerResponderIntegration(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)

	s := startResponderTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	sub, err := RegisterOwnerResponder(conn, pool, nil)
	if err != nil {
		t.Fatalf("register owner responder: %v", err)
	}
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
	})

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	ledger := NewLedger(pool, nil)
	claimResp := ledger.Claim(context.Background(), req)
	if claimResp.Decision != DecisionGranted {
		t.Fatalf("setup claim: want granted, got %+v", claimResp)
	}

	t.Run("known run", func(t *testing.T) {
		body, _ := json.Marshal(ownerLookupRequest{RunID: claimResp.RunID})
		msg, err := conn.Request(bus.LedgerOwnerSubject, body, 5*time.Second)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var resp ownerLookupResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !resp.Found {
			t.Fatalf("want found=true, got %+v", resp)
		}
		if resp.Cluster != req.Cluster || resp.Namespace != req.Namespace || resp.Cell != req.Cell || resp.Instance != req.CellInstance {
			t.Fatalf("owner tuple mismatch: want (%s,%s,%s,%s), got (%s,%s,%s,%s)",
				req.Cluster, req.Namespace, req.Cell, req.CellInstance,
				resp.Cluster, resp.Namespace, resp.Cell, resp.Instance)
		}
		// No runs row exists yet (no run.started ingested), so status is
		// empty and no anchor is reported.
		if resp.Status != "" {
			t.Fatalf("want empty status with no runs row, got %q", resp.Status)
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		body, _ := json.Marshal(ownerLookupRequest{RunID: newTestUUIDv7String()})
		msg, err := conn.Request(bus.LedgerOwnerSubject, body, 5*time.Second)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var resp ownerLookupResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Found {
			t.Fatalf("want found=false for an unknown run, got %+v", resp)
		}
	})

	t.Run("timeout answers nothing (no subscriber)", func(t *testing.T) {
		// A request on a subject no responder ever subscribes to times out,
		// the same failure shape a gateway sees when the recorder is
		// unreachable.
		_, err := conn.Request("varroa.ledger.owner.unsubscribed", []byte(`{}`), 200*time.Millisecond)
		if err == nil {
			t.Fatal("want a timeout error, got a reply")
		}
	})
}

// TestHandleOwnerLookupDatabaseErrorAnswersErrorNotFound proves
// design.md D4's distinction directly: a database error must never
// answer the same shape as a genuine miss. handleOwnerLookup is
// exercised directly against a closed pool, so every query fails
// deterministically without needing a NATS server.
func TestHandleOwnerLookupDatabaseErrorAnswersErrorNotFound(t *testing.T) {
	t.Parallel()

	dsn := requireTestDSN(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	pool.Close()

	body, _ := json.Marshal(ownerLookupRequest{RunID: newTestUUIDv7String()})
	out := handleOwnerLookup(slog.Default(), pool, body)

	var resp ownerLookupResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Found {
		t.Fatalf("want found=false on a database error, got %+v", resp)
	}
	if resp.Error == "" {
		t.Fatal("want a non-empty error on a database error, so the gateway never caches it as a foreign-run refusal")
	}
}
