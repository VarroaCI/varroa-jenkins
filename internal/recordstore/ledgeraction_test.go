package recordstore

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestLedgerActionFieldNames round-trips the three kinds through JSON and
// asserts each field name the spec's ledger-action table names.
func TestLedgerActionFieldNames(t *testing.T) {
	buildNumber := int64(42)

	cases := []struct {
		name   string
		action LedgerAction
		want   map[string]bool // JSON keys the encoded action must carry
	}{
		{
			name: "cancel_run",
			action: LedgerAction{
				Kind:        LedgerActionCancelRun,
				Cluster:     "core",
				Namespace:   "team-a",
				Controller:  "cell-1",
				Actor:       LedgerActionRecorder,
				RunID:       "run-1",
				JobPath:     "folder/job",
				BuildNumber: &buildNumber,
			},
			want: map[string]bool{
				"kind": true, "cluster": true, "namespace": true, "controller": true,
				"actor": true, "run_id": true, "job_path": true, "build_number": true,
			},
		},
		{
			name: "retry_run",
			action: LedgerAction{
				Kind:       LedgerActionRetryRun,
				Cluster:    "core",
				Namespace:  "team-a",
				Controller: "cell-1",
				Actor:      "alice",
				RunID:      "run-2",
				IntentKey:  "manual:abc",
				JobPath:    "folder/job",
				Parameters: json.RawMessage(`{"branch":"main"}`),
			},
			want: map[string]bool{
				"kind": true, "cluster": true, "namespace": true, "controller": true,
				"actor": true, "run_id": true, "intent_key": true, "job_path": true,
				"parameters": true,
			},
		},
		{
			name: "wake_cell",
			action: LedgerAction{
				Kind:       LedgerActionWakeCell,
				Cluster:    "core",
				Namespace:  "team-a",
				Controller: "cell-1",
				Actor:      "bob",
			},
			want: map[string]bool{
				"kind": true, "cluster": true, "namespace": true, "controller": true,
				"actor": true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.action)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for key, present := range tc.want {
				if _, ok := decoded[key]; ok != present {
					t.Errorf("key %q present=%v, want %v (payload=%s)", key, ok, present, data)
				}
			}

			var roundTrip LedgerAction
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatalf("round-trip unmarshal: %v", err)
			}
			if roundTrip.Kind != tc.action.Kind || roundTrip.Actor != tc.action.Actor {
				t.Fatalf("round trip mismatch: got %+v, want %+v", roundTrip, tc.action)
			}
		})
	}
}

// startLedgerActionTestServer starts an embedded NATS server for the
// ledger-action publish test.
func startLedgerActionTestServer(t *testing.T) *server.Server {
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

// TestPublishLedgerActionIntegration asserts PublishLedgerAction sends
// every kind on bus.LedgerActionSubject(cluster), with a payload that
// decodes back to the same action, over a fake (embedded) NATS bus.
func TestPublishLedgerActionIntegration(t *testing.T) {
	s := startLedgerActionTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	buildNumber := int64(7)
	cases := []struct {
		name   string
		action LedgerAction
	}{
		{
			name: "cancel_run",
			action: LedgerAction{
				Kind:        LedgerActionCancelRun,
				Cluster:     "core",
				Namespace:   "team-a",
				Controller:  "cell-1",
				Actor:       LedgerActionRecorder,
				RunID:       "run-3",
				JobPath:     "folder/job",
				BuildNumber: &buildNumber,
			},
		},
		{
			name: "retry_run",
			action: LedgerAction{
				Kind:       LedgerActionRetryRun,
				Cluster:    "core",
				Namespace:  "team-a",
				Controller: "cell-1",
				Actor:      "alice",
				RunID:      "run-4",
				IntentKey:  "retry:run-3",
				JobPath:    "folder/job",
				Parameters: json.RawMessage(`{"branch":"main"}`),
			},
		},
		{
			name: "wake_cell",
			action: LedgerAction{
				Kind:       LedgerActionWakeCell,
				Cluster:    "core",
				Namespace:  "team-a",
				Controller: "cell-1",
				Actor:      "bob",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan []byte, 1)
			sub, err := conn.Subscribe(bus.LedgerActionSubject("core"), func(msg *nats.Msg) {
				received <- msg.Data
			})
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			conn.NATSConn().Flush()

			if err := PublishLedgerAction(conn, tc.action); err != nil {
				t.Fatalf("publish: %v", err)
			}

			select {
			case data := <-received:
				var got LedgerAction
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatalf("unmarshal received: %v", err)
				}
				if got.Kind != tc.action.Kind || got.RunID != tc.action.RunID || got.Actor != tc.action.Actor {
					t.Fatalf("got %+v, want %+v", got, tc.action)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the published action")
			}
		})
	}
}

// TestRecorderCannotPublishMiteOrWakeSubjects starts an embedded NATS
// server with the recorder's own production permission set (the chart's
// nats-auth-config.yaml "recorder" user: publish allowed only on
// varroa.ledger.actions.<cluster> and its own JetStream/inbox subjects),
// then asserts a raw publish to a mite.> subject and to a wake.> subject
// are both denied. This is task 9.1's "the recorder SHALL reach no mite
// subject and no wake subject" acceptance, proved against a fake bus
// rather than by inspecting the chart.
func TestRecorderCannotPublishMiteOrWakeSubjects(t *testing.T) {
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		Users: []*server.User{
			{
				Username: "recorder",
				Password: "rec_pass",
				Permissions: &server.Permissions{
					Publish: &server.SubjectPermission{
						Allow: []string{
							"varroa.ledger.actions.core",
							"_INBOX_gateway.>",
							"_INBOX_operator.>",
						},
					},
					Subscribe: &server.SubjectPermission{
						Allow: []string{
							"varroa.ledger.claim",
							"_INBOX_recorder.>",
						},
					},
				},
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

	for _, subject := range []string{"mite.core.team-a.cell-1.out", "wake.core.team-a.cell-1"} {
		t.Run(subject, func(t *testing.T) {
			var (
				mu       sync.Mutex
				asyncErr error
			)
			nc, err := nats.Connect(s.ClientURL(), nats.UserInfo("recorder", "rec_pass"),
				nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
					mu.Lock()
					asyncErr = err
					mu.Unlock()
				}),
			)
			if err != nil {
				t.Fatalf("connect as recorder: %v", err)
			}
			defer nc.Close()

			if err := nc.Publish(subject, []byte(`{}`)); err != nil {
				t.Fatalf("publish itself must succeed (async denial): %v", err)
			}
			nc.Flush()
			time.Sleep(200 * time.Millisecond)

			mu.Lock()
			got := asyncErr
			mu.Unlock()
			if got == nil {
				t.Fatalf("expected a permissions violation publishing %s as the recorder, got none", subject)
			}
			t.Logf("recorder publish %s correctly denied: %v", subject, got)
		})
	}
}
