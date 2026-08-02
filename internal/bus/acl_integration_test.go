//go:build natsintegration

// Package bus integration test — per-service $JS.API.* / $KV.* subject set.
//
// This file records the provisional JetStream/KV permission subjects per
// service, as enumerated in design.md D4. These patterns are the source of
// truth for the chart authorization block (task 3.2) and are validated by the
// integration test body (task 4.1) against a real nats-server. The binding
// contract is the test — if any pattern proves insufficient or over-broad after
// running against the real server, this comment and the chart must be updated
// together so they never diverge.
//
// ┌────────────────────────────────────────────────────────────────────────────┐
// │ Per-service $JS.API.* / $KV.* subject set  (design D4, provisional)       │
// ├────────────────────────────────────────────────────────────────────────────┤
// │                                                                             │
// │  operator:                                                                  │
// │    publish:                                                                 │
// │      $KV.mite_desired.>                                                     │
// │      $JS.API.STREAM.*.KV_mite_desired                                       │
// │      $JS.API.STREAM.MSG.*.KV_mite_desired                                   │
// │      $JS.API.STREAM.MSG.*.KV_mite_desired.>                                 │
// │      $JS.API.DIRECT.GET.KV_mite_desired                                     │
// │      $JS.API.DIRECT.GET.KV_mite_desired.>                                   │
// │      $JS.API.CONSUMER.CREATE.KV_mite_desired                                │
// │      $JS.API.CONSUMER.CREATE.KV_mite_desired.>                              │
// │      $JS.API.CONSUMER.*.KV_mite_desired.>                                   │
// │      $JS.API.STREAM.*.KV_mite_snapshots                                     │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots                                 │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots.>                               │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots                                   │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots.>                                 │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots                              │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots.>                            │
// │      $JS.API.CONSUMER.*.KV_mite_snapshots.>                                 │
// │      $JS.API.STREAM.*.KV_mite_presence                                      │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence                                  │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence.>                                │
// │      $JS.API.DIRECT.GET.KV_mite_presence                                    │
// │      $JS.API.DIRECT.GET.KV_mite_presence.>                                  │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence                               │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence.>                             │
// │      $JS.API.CONSUMER.*.KV_mite_presence.>                                  │
// │    subscribe:                                                               │
// │      mite.*.*.in                                                            │
// │      $KV.mite_snapshots.>                                                   │
// │      $KV.mite_presence.>                                                    │
// │      _INBOX_operator.>                                                      │
// │                                                                             │
// │  gateway:                                                                   │
// │    publish:                                                                 │
// │      mite.*.*.in                                                            │
// │      $KV.mite_snapshots.>                                                   │
// │      $KV.mite_presence.>                                                    │
// │      $KV.varroa_consumed_tokens.>                                           │
// │      events.brood                                                           │
// │      events.activity                                                        │
// │      _INBOX_operator.>                                                      │
// │      _INBOX_bff.>                                                           │
// │      $JS.API.STREAM.*.varroa                                                │
// │      $JS.API.CONSUMER.CREATE.varroa.>                                       │
// │      $JS.API.CONSUMER.*.varroa.>                                            │
// │      $JS.API.STREAM.*.KV_mite_desired                                       │
// │      $JS.API.STREAM.MSG.*.KV_mite_desired                                   │
// │      $JS.API.STREAM.MSG.*.KV_mite_desired.>                                 │
// │      $JS.API.DIRECT.GET.KV_mite_desired                                     │
// │      $JS.API.DIRECT.GET.KV_mite_desired.>                                   │
// │      $JS.API.CONSUMER.CREATE.KV_mite_desired                                │
// │      $JS.API.CONSUMER.CREATE.KV_mite_desired.>                              │
// │      $JS.API.CONSUMER.*.KV_mite_desired.>                                   │
// │      $JS.API.STREAM.*.KV_mite_snapshots                                     │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots                                 │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots.>                               │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots                                   │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots.>                                 │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots                              │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots.>                            │
// │      $JS.API.CONSUMER.*.KV_mite_snapshots.>                                 │
// │      $JS.API.STREAM.*.KV_mite_presence                                      │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence                                  │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence.>                                │
// │      $JS.API.DIRECT.GET.KV_mite_presence                                    │
// │      $JS.API.DIRECT.GET.KV_mite_presence.>                                  │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence                               │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence.>                             │
// │      $JS.API.CONSUMER.*.KV_mite_presence.>                                  │
// │      $JS.API.STREAM.*.KV_varroa_consumed_tokens                           │
// │      $JS.API.STREAM.MSG.*.KV_varroa_consumed_tokens                       │
// │      $JS.API.STREAM.MSG.*.KV_varroa_consumed_tokens.>                     │
// │      $JS.API.DIRECT.GET.KV_varroa_consumed_tokens                         │
// │      $JS.API.DIRECT.GET.KV_varroa_consumed_tokens.>                       │
// │      $JS.API.CONSUMER.CREATE.KV_varroa_consumed_tokens                    │
// │      $JS.API.CONSUMER.CREATE.KV_varroa_consumed_tokens.>                  │
// │      $JS.API.CONSUMER.*.KV_varroa_consumed_tokens.>                       │
// │    subscribe:                                                               │
// │      $KV.mite_desired.>                                                     │
// │      mite.*.*.content                                                       │
// │      _INBOX_gateway.>                                                       │
// │                                                                             │
// │  bff:                                                                       │
// │    publish:                                                                 │
// │      mite.*.*.content                                                       │
// │      $JS.API.STREAM.*.KV_mite_snapshots                                     │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots                                 │
// │      $JS.API.STREAM.MSG.*.KV_mite_snapshots.>                               │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots                                   │
// │      $JS.API.DIRECT.GET.KV_mite_snapshots.>                                 │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots                              │
// │      $JS.API.CONSUMER.CREATE.KV_mite_snapshots.>                            │
// │      $JS.API.CONSUMER.*.KV_mite_snapshots.>                                 │
// │      $JS.API.STREAM.*.KV_mite_presence                                      │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence                                  │
// │      $JS.API.STREAM.MSG.*.KV_mite_presence.>                                │
// │      $JS.API.DIRECT.GET.KV_mite_presence                                    │
// │      $JS.API.DIRECT.GET.KV_mite_presence.>                                  │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence                               │
// │      $JS.API.CONSUMER.CREATE.KV_mite_presence.>                             │
// │      $JS.API.CONSUMER.*.KV_mite_presence.>                                  │
// │    subscribe:                                                               │
// │      events.brood                                                           │
// │      events.activity                                                        │
// │      $KV.mite_snapshots.>                                                   │
// │      $KV.mite_presence.>                                                    │
// │      _INBOX_bff.>                                                           │
// │                                                                             │
// │  NO service is granted:                                                     │
// │      $JS.API.STREAM.NAMES, $JS.API.STREAM.LIST                              │
// │      $JS.API.CONSUMER.NAMES.*, $JS.API.CONSUMER.LIST.*                     │
// │      $KV.mite_desired.> (sub for non-gateway, pub for non-operator)         │
// └────────────────────────────────────────────────────────────────────────────┘
package bus

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startACLTestServer starts an embedded nats-server with the same single-account,
// three-user permission model the chart renders (design D3/D4).
func startACLTestServer(t *testing.T) *server.Server {
	t.Helper()

	makePerms := func(pubAllow, subAllow []string) *server.Permissions {
		return &server.Permissions{
			Publish:   &server.SubjectPermission{Allow: pubAllow},
			Subscribe: &server.SubjectPermission{Allow: subAllow},
		}
	}

	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		Users: []*server.User{
			{
				Username: "operator",
				Password: "op_pass",
				Permissions: makePerms(
					// publish allow-list (D3 + D4)
					[]string{
						"$JS.API.INFO",
						"$KV.mite_desired.>",
						// KV_mite_desired JS API
						"$JS.API.STREAM.*.KV_mite_desired",
						"$JS.API.STREAM.MSG.*.KV_mite_desired",
						"$JS.API.STREAM.MSG.*.KV_mite_desired.>",
						"$JS.API.DIRECT.GET.KV_mite_desired",
						"$JS.API.DIRECT.GET.KV_mite_desired.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_desired",
						"$JS.API.CONSUMER.CREATE.KV_mite_desired.>",
						"$JS.API.CONSUMER.*.KV_mite_desired.>",
						// KV_mite_snapshots JS API
						"$JS.API.STREAM.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots.>",
						"$JS.API.DIRECT.GET.KV_mite_snapshots",
						"$JS.API.DIRECT.GET.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.*.KV_mite_snapshots.>",
						// KV_mite_presence JS API
						"$JS.API.STREAM.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence.>",
						"$JS.API.DIRECT.GET.KV_mite_presence",
						"$JS.API.DIRECT.GET.KV_mite_presence.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence.>",
						"$JS.API.CONSUMER.*.KV_mite_presence.>",
					},
					// subscribe allow-list
					[]string{
						"mite.*.*.*.in",
						"$KV.mite_snapshots.>",
						"$KV.mite_presence.>",
						"_INBOX_operator.>",
					},
				),
			},
			{
				Username: "gateway",
				Password: "gw_pass",
				Permissions: makePerms(
					[]string{
						"$JS.API.INFO",
						"mite.*.*.*.in",
						"$KV.mite_snapshots.>",
						"$KV.mite_presence.>",
						"$KV.varroa_consumed_tokens.>",
						"events.brood.*",
						"_INBOX_operator.>",
						"_INBOX_bff.>",
						// varroa stream JS API
						"$JS.API.STREAM.*.varroa",
						"$JS.API.CONSUMER.CREATE.varroa.>",
						"$JS.API.CONSUMER.*.varroa.>",
						// KV_mite_desired JS API
						"$JS.API.STREAM.*.KV_mite_desired",
						"$JS.API.STREAM.MSG.*.KV_mite_desired",
						"$JS.API.STREAM.MSG.*.KV_mite_desired.>",
						"$JS.API.DIRECT.GET.KV_mite_desired",
						"$JS.API.DIRECT.GET.KV_mite_desired.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_desired",
						"$JS.API.CONSUMER.CREATE.KV_mite_desired.>",
						"$JS.API.CONSUMER.*.KV_mite_desired.>",
						// KV_mite_snapshots JS API
						"$JS.API.STREAM.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots.>",
						"$JS.API.DIRECT.GET.KV_mite_snapshots",
						"$JS.API.DIRECT.GET.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.*.KV_mite_snapshots.>",
						// KV_mite_presence JS API
						"$JS.API.STREAM.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence.>",
						"$JS.API.DIRECT.GET.KV_mite_presence",
						"$JS.API.DIRECT.GET.KV_mite_presence.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence.>",
						"$JS.API.CONSUMER.*.KV_mite_presence.>",
						// KV_varroa_consumed_tokens JS API (bootstrap single-use)
						"$JS.API.STREAM.*.KV_varroa_consumed_tokens",
						"$JS.API.STREAM.MSG.*.KV_varroa_consumed_tokens",
						"$JS.API.STREAM.MSG.*.KV_varroa_consumed_tokens.>",
						"$JS.API.DIRECT.GET.KV_varroa_consumed_tokens",
						"$JS.API.DIRECT.GET.KV_varroa_consumed_tokens.>",
						"$JS.API.CONSUMER.CREATE.KV_varroa_consumed_tokens",
						"$JS.API.CONSUMER.CREATE.KV_varroa_consumed_tokens.>",
						"$JS.API.CONSUMER.*.KV_varroa_consumed_tokens.>",
					},
					[]string{
						"$KV.mite_desired.>",
						"mite.*.*.*.content",
						"_INBOX_gateway.>",
					},
				),
			},
			{
				Username: "bff",
				Password: "bff_pass",
				Permissions: makePerms(
					[]string{
						"$JS.API.INFO",
						"mite.*.*.*.content",
						// KV_mite_snapshots JS API
						"$JS.API.STREAM.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots",
						"$JS.API.STREAM.MSG.*.KV_mite_snapshots.>",
						"$JS.API.DIRECT.GET.KV_mite_snapshots",
						"$JS.API.DIRECT.GET.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots",
						"$JS.API.CONSUMER.CREATE.KV_mite_snapshots.>",
						"$JS.API.CONSUMER.*.KV_mite_snapshots.>",
						// KV_mite_presence JS API
						"$JS.API.STREAM.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence",
						"$JS.API.STREAM.MSG.*.KV_mite_presence.>",
						"$JS.API.DIRECT.GET.KV_mite_presence",
						"$JS.API.DIRECT.GET.KV_mite_presence.>",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence",
						"$JS.API.CONSUMER.CREATE.KV_mite_presence.>",
						"$JS.API.CONSUMER.*.KV_mite_presence.>",
					},
					[]string{
						"events.brood.>",
						"$KV.mite_snapshots.>",
						"$KV.mite_presence.>",
						"_INBOX_bff.>",
					},
				),
			},
		},
	}
	s, err := server.NewServer(opts)
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

// connectACL connects as a specific user with the given inbox prefix.
func connectACL(t *testing.T, url, user, pass, inboxPrefix string) *Conn {
	t.Helper()
	conn, err := Connect(url, Config{
		Username:    user,
		Password:    pass,
		InboxPrefix: inboxPrefix,
	})
	if err != nil {
		t.Fatalf("connect %s: %v", user, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// --------------------------------------------------------------------------
// ALLOW assertions — each identity's expected operations MUST succeed.
// --------------------------------------------------------------------------

func TestACLIntegration(t *testing.T) {
	srv := startACLTestServer(t)
	url := srv.ClientURL()

	// --- Connect all three identities ---
	op := connectACL(t, url, "operator", "op_pass", "_INBOX_operator")
	gw := connectACL(t, url, "gateway", "gw_pass", "_INBOX_gateway")
	bf := connectACL(t, url, "bff", "bff_pass", "_INBOX_bff")

	// -----------------------------------------------------------------------
	// ALLOW: operator can create and Put to mite_desired KV
	// -----------------------------------------------------------------------
	t.Run("operator_put_desired", func(t *testing.T) {
		kv, err := op.EnsureKV(KVDesiredBucket, 0)
		if err != nil {
			t.Fatalf("operator EnsureKV(mite_desired): %v", err)
		}
		if err := kv.PutString("ns1/ctrl1", `{"desired":"running"}`); err != nil {
			t.Fatalf("operator Put to mite_desired: %v", err)
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: gateway can Watch mite_desired KV
	// -----------------------------------------------------------------------
	t.Run("gateway_watch_desired", func(t *testing.T) {
		kv, err := gw.EnsureKV(KVDesiredBucket, 0)
		if err != nil {
			t.Fatalf("gateway EnsureKV(mite_desired): %v", err)
		}
		watcher, err := kv.Watch("ns1/ctrl1")
		if err != nil {
			t.Fatalf("gateway Watch(mite_desired): %v", err)
		}
		defer watcher.Stop()
		select {
		case entry := <-watcher.Updates():
			if entry == nil {
				t.Fatal("expected non-nil entry from Watch")
			}
			t.Logf("gateway received desired update: %s", string(entry.Value()))
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for Watch update")
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: gateway can create the varroa stream (EnsureStream)
	// -----------------------------------------------------------------------
	t.Run("gateway_ensure_stream_varroa", func(t *testing.T) {
		cfg := StreamConfig(DefaultStreamName)
		if err := gw.EnsureStream(cfg); err != nil {
			t.Fatalf("gateway EnsureStream(varroa): %v", err)
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: gateway and bff share mite_snapshots KV (operator creates via JS,
	// gateway writes and reads, bff reads)
	// -----------------------------------------------------------------------
	t.Run("gateway_and_bff_snapshots", func(t *testing.T) {
		// Operator creates the KV bucket (create+read per D4).
		opKV, err := op.EnsureKV(KVSnapshotBucket, 5*time.Minute)
		if err != nil {
			t.Fatalf("operator EnsureKV(mite_snapshots): %v", err)
		}
		// Gateway can write and read.
		gwKV, err := gw.EnsureKV(KVSnapshotBucket, 5*time.Minute)
		if err != nil {
			t.Fatalf("gateway EnsureKV(mite_snapshots): %v", err)
		}
		if err := gwKV.PutString("ns1/ctrl1", `{"snap":"gw-data"}`); err != nil {
			t.Fatalf("gateway Put to mite_snapshots: %v", err)
		}
		val, err := gwKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("gateway Get from mite_snapshots: %v", err)
		}
		if val == nil {
			t.Fatal("expected non-nil snapshot value for gateway")
		}
		// BFF can read.
		bffKV, err := bf.EnsureKV(KVSnapshotBucket, 5*time.Minute)
		if err != nil {
			t.Fatalf("bff EnsureKV(mite_snapshots): %v", err)
		}
		val2, err := bffKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("bff Get from mite_snapshots: %v", err)
		}
		if val2 == nil {
			t.Fatal("expected non-nil snapshot value for bff")
		}
		// Operator can also read (via subscribe + JS API).
		val3, err := opKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("operator Get from mite_snapshots: %v", err)
		}
		if val3 == nil {
			t.Fatal("expected non-nil snapshot value for operator")
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: gateway and bff share mite_presence KV
	// -----------------------------------------------------------------------
	t.Run("gateway_and_bff_presence", func(t *testing.T) {
		opKV, err := op.EnsureKV(KVPresenceBucket, 90*time.Second)
		if err != nil {
			t.Fatalf("operator EnsureKV(mite_presence): %v", err)
		}
		gwKV, err := gw.EnsureKV(KVPresenceBucket, 90*time.Second)
		if err != nil {
			t.Fatalf("gateway EnsureKV(mite_presence): %v", err)
		}
		if err := gwKV.PutString("ns1/ctrl1", `{"presence":"alive"}`); err != nil {
			t.Fatalf("gateway Put to mite_presence: %v", err)
		}
		val, err := gwKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("gateway Get from mite_presence: %v", err)
		}
		if val == nil {
			t.Fatal("expected non-nil presence value for gateway")
		}
		// BFF can read.
		bffKV, err := bf.EnsureKV(KVPresenceBucket, 90*time.Second)
		if err != nil {
			t.Fatalf("bff EnsureKV(mite_presence): %v", err)
		}
		val2, err := bffKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("bff Get from mite_presence: %v", err)
		}
		if val2 == nil {
			t.Fatal("expected non-nil presence value for bff")
		}
		// Operator can also read (via subscribe + JS API).
		val3, err := opKV.Get("ns1/ctrl1")
		if err != nil {
			t.Fatalf("operator Get from mite_presence: %v", err)
		}
		if val3 == nil {
			t.Fatal("expected non-nil presence value for operator")
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: operator→gateway content request/reply
	// -----------------------------------------------------------------------
	t.Run("operator_gateway_content_request_reply", func(t *testing.T) {
		contentSubj := MiteContentSubject(DefaultCluster, "ns1", "ctrl1")

		// Gateway subscribes as content responder.
		_, err := gw.SubscribeRequest(contentSubj, "", func(data []byte) []byte {
			return append([]byte("reply-"), data...)
		})
		if err != nil {
			t.Fatalf("gateway SubscribeRequest: %v", err)
		}
		// Flush to ensure the subscription is registered server-side.
		gw.NATSConn().Flush()

		// Operator sends request.
		resp, err := op.Request(contentSubj, []byte("ping"), 3*time.Second)
		if err != nil {
			t.Fatalf("operator content request: %v", err)
		}
		if string(resp.Data) != "reply-ping" {
			t.Fatalf("expected 'reply-ping', got %q", string(resp.Data))
		}
	})

	// -----------------------------------------------------------------------
	// ALLOW: events publish — gateway events.brood.*, bff subscribes events.brood.>
	// -----------------------------------------------------------------------
	t.Run("events_publish", func(t *testing.T) {
		if err := gw.Publish("events.brood.core", []byte(`{"event":"gw-test"}`)); err != nil {
			t.Fatalf("gateway publish to events.brood.*: %v", err)
		}
		// BFF can subscribe to events.brood.>.
		sub, err := bf.SubscribeData("events.brood.>", func(data []byte) {})
		if err != nil {
			t.Fatalf("bff subscribe events.brood.>: %v", err)
		}
		sub.Unsubscribe()
	})

	// -----------------------------------------------------------------------
	// ALLOW: bff publishes mite.*.*.content (content requests)
	// -----------------------------------------------------------------------
	t.Run("bff_content_publish", func(t *testing.T) {
		contentSubj := MiteContentSubject(DefaultCluster, "ns2", "ctrl2")

		// Gateway subscribes as responder.
		_, err := gw.SubscribeRequest(contentSubj, "", func(data []byte) []byte {
			return []byte("bff-reply")
		})
		if err != nil {
			t.Fatalf("gateway SubscribeRequest for bff content: %v", err)
		}
		gw.NATSConn().Flush()

		// BFF sends request with its own inbox prefix.
		resp, err := bf.Request(contentSubj, []byte("hello"), 3*time.Second)
		if err != nil {
			t.Fatalf("bff content request: %v", err)
		}
		if string(resp.Data) != "bff-reply" {
			t.Fatalf("expected 'bff-reply', got %q", string(resp.Data))
		}
	})

	// -----------------------------------------------------------------------
	// DENY: bff Put to mite_desired KV must be refused
	// -----------------------------------------------------------------------
	t.Run("deny_bff_put_desired", func(t *testing.T) {
		// bff is not granted $KV.mite_desired.> nor the JS API for KV_mite_desired.
		_, err := bf.EnsureKV(KVDesiredBucket, 0)
		if err == nil {
			t.Fatal("expected error for bff EnsureKV(mite_desired), got nil")
		}
		t.Logf("bff EnsureKV(mite_desired) correctly denied: %v", err)
	})

	// -----------------------------------------------------------------------
	// DENY: operator subscribing to $KV.mite_desired.> is not allowed
	// (permissions violations are delivered asynchronously by nats.go)
	// -----------------------------------------------------------------------
	t.Run("deny_operator_sub_desired", func(t *testing.T) {
		var (
			asyncMu  sync.Mutex
			asyncErr error
		)
		nc, err := nats.Connect(url, nats.UserInfo("operator", "op_pass"),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
				asyncMu.Lock()
				asyncErr = err
				asyncMu.Unlock()
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()

		// Subscribe to a subject not in operator's allow-list.
		sub, err := nc.Subscribe("$KV.mite_desired.>", func(_ *nats.Msg) {})
		if err != nil {
			t.Fatalf("Subscribe itself must succeed (async denial): %v", err)
		}
		defer sub.Unsubscribe()

		// Give the server time to reject the subscription.
		nc.Flush()
		time.Sleep(200 * time.Millisecond)

		asyncMu.Lock()
		got := asyncErr
		asyncMu.Unlock()
		if got == nil {
			t.Fatal("expected async permissions violation for operator subscribe $KV.mite_desired.>")
		}
		t.Logf("operator subscribe $KV.mite_desired.> correctly denied: %v", got)
	})

	// -----------------------------------------------------------------------
	// DENY: operator publishing to 4-token mite subject (old format) is denied
	// -----------------------------------------------------------------------
	t.Run("deny_operator_pub_old_mite_subject", func(t *testing.T) {
		var (
			asyncMu  sync.Mutex
			asyncErr error
		)
		nc, err := nats.Connect(url, nats.UserInfo("operator", "op_pass"),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
				asyncMu.Lock()
				asyncErr = err
				asyncMu.Unlock()
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()

		// Publish to a 4-token mite subject (old format without cluster).
		if err := nc.Publish("mite.a.b.out", []byte(`{"event":"bad"}`)); err != nil {
			t.Fatalf("Publish itself must succeed (async denial): %v", err)
		}
		nc.Flush()
		time.Sleep(200 * time.Millisecond)

		asyncMu.Lock()
		got := asyncErr
		asyncMu.Unlock()
		if got == nil {
			t.Fatal("expected async permissions violation for operator publish to mite.a.b.out")
		}
		t.Logf("operator publish to mite.a.b.out correctly denied: %v", got)
	})

	// -----------------------------------------------------------------------
	// DENY: bff subscribing to _INBOX_operator.> (cross-service inbox)
	// -----------------------------------------------------------------------
	t.Run("deny_bff_sub_operator_inbox", func(t *testing.T) {
		var (
			asyncMu  sync.Mutex
			asyncErr error
		)
		nc, err := nats.Connect(url, nats.UserInfo("bff", "bff_pass"),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
				asyncMu.Lock()
				asyncErr = err
				asyncMu.Unlock()
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()

		sub, err := nc.Subscribe("_INBOX_operator.>", func(_ *nats.Msg) {})
		if err != nil {
			t.Fatalf("Subscribe itself must succeed (async denial): %v", err)
		}
		defer sub.Unsubscribe()

		nc.Flush()
		time.Sleep(200 * time.Millisecond)

		asyncMu.Lock()
		got := asyncErr
		asyncMu.Unlock()
		if got == nil {
			t.Fatal("expected async permissions violation for bff subscribe _INBOX_operator.>")
		}
		t.Logf("bff subscribe _INBOX_operator.> correctly denied: %v", got)
	})

	// -----------------------------------------------------------------------
	// DENY: connection with invalid password is refused (synchronous)
	// -----------------------------------------------------------------------
	t.Run("deny_invalid_password", func(t *testing.T) {
		_, err := Connect(url, Config{
			Username: "operator",
			Password: "wrong_password",
		})
		if err == nil {
			t.Fatal("expected error connecting with wrong password, got nil")
		}
		t.Logf("invalid password correctly rejected: %v", err)
	})

	// -----------------------------------------------------------------------
	// ALLOW: gateway creates + writes the consumed-tokens bucket (bootstrap
	// single-use); Create is atomic create-only, second call must key-exist.
	// -----------------------------------------------------------------------
	t.Run("gateway_consumed_tokens_create", func(t *testing.T) {
		kv, err := gw.EnsureKV(KVConsumedTokensBucket, time.Minute)
		if err != nil {
			t.Fatalf("gateway EnsureKV(varroa_consumed_tokens): %v", err)
		}
		if err := kv.Create("jti-acl-test", []byte(`{"ts":"t"}`)); err != nil {
			t.Fatalf("gateway Create in consumed-tokens bucket: %v", err)
		}
		if err := kv.Create("jti-acl-test", []byte(`{"ts":"t"}`)); !errors.Is(err, ErrKVKeyExists) {
			t.Fatalf("second Create: want ErrKVKeyExists, got %v", err)
		}
	})

	// -----------------------------------------------------------------------
	// DENY: operator and bff publishing to $KV.varroa_consumed_tokens.> is
	// not allowed (async violation, same delivery as other publish denials)
	// -----------------------------------------------------------------------
	for _, tc := range []struct{ user, pass string }{
		{"operator", "op_pass"},
		{"bff", "bff_pass"},
	} {
		t.Run("deny_"+tc.user+"_pub_consumed_tokens", func(t *testing.T) {
			var (
				mu       sync.Mutex
				asyncErr error
			)
			nc, err := nats.Connect(url, nats.UserInfo(tc.user, tc.pass),
				nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
					mu.Lock()
					asyncErr = err
					mu.Unlock()
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()

			if err := nc.Publish("$KV.varroa_consumed_tokens.jti-x", []byte("v")); err != nil {
				t.Fatalf("Publish itself must succeed (async denial): %v", err)
			}
			nc.Flush()
			time.Sleep(200 * time.Millisecond)

			mu.Lock()
			got := asyncErr
			mu.Unlock()
			if got == nil {
				t.Fatalf("expected async permissions violation for %s publish $KV.varroa_consumed_tokens.>", tc.user)
			}
			t.Logf("%s publish $KV.varroa_consumed_tokens.> correctly denied: %v", tc.user, got)
		})
	}
}
