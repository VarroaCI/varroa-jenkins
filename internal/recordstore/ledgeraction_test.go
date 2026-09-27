package recordstore

import (
	"encoding/json"
	"errors"
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

// startRecorderACLServer starts an embedded NATS server with the
// recorder's production permission set (the chart's nats-auth-config.yaml
// "recorder" user) and an operator user that reads ledger actions. The
// recorder's ledger-action grant is the wildcard, because one recorder
// serves every cluster of the brood.
func startRecorderACLServer(t *testing.T) *server.Server {
	t.Helper()
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
							"varroa.ledger.actions.>",
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
			{
				Username: "operator",
				Password: "op_pass",
				Permissions: &server.Permissions{
					Subscribe: &server.SubjectPermission{
						Allow: []string{"varroa.ledger.actions.>"},
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
	return s
}

// connectRecorderRaw connects as the recorder and returns a function that
// answers the last asynchronous error the server reported.
func connectRecorderRaw(t *testing.T, s *server.Server) (*nats.Conn, func() error) {
	t.Helper()
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
	t.Cleanup(nc.Close)
	return nc, func() error {
		mu.Lock()
		defer mu.Unlock()
		return asyncErr
	}
}

// TestRecorderCannotPublishMiteOrWakeSubjects asserts the recorder's
// production grant denies a raw publish to a mite.> subject and to a
// wake.> subject. The recorder reaches a controller only through the
// operator's ledger-action subscriber.
func TestRecorderCannotPublishMiteOrWakeSubjects(t *testing.T) {
	s := startRecorderACLServer(t)

	for _, subject := range []string{"mite.core.team-a.cell-1.out", "wake.core.team-a.cell-1"} {
		t.Run(subject, func(t *testing.T) {
			nc, lastErr := connectRecorderRaw(t, s)
			if err := nc.Publish(subject, []byte(`{}`)); err != nil {
				t.Fatalf("publish itself must succeed (async denial): %v", err)
			}
			waitPermissionViolation(t, lastErr, "publish "+subject)
		})
	}
}

// waitPermissionViolation waits until lastErr answers a permission
// violation, and fails the test after five seconds.
func waitPermissionViolation(t *testing.T, lastErr func() error, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := lastErr(); got != nil {
			if !errors.Is(got, nats.ErrPermissionViolation) {
				t.Fatalf("%s: want a permission violation, got %v", what, got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: expected the server to deny the recorder, got no error", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRecorderPublishesLedgerActionForMemberCluster asserts the recorder
// publishes a ledger action for a member cluster that runs no recorder,
// and that the member's operator receives it. It also asserts the
// recorder cannot consume that subject.
func TestRecorderPublishesLedgerActionForMemberCluster(t *testing.T) {
	s := startRecorderACLServer(t)

	operator, err := bus.Connect(s.ClientURL(), bus.Config{Username: "operator", Password: "op_pass"})
	if err != nil {
		t.Fatalf("connect as operator: %v", err)
	}
	t.Cleanup(operator.Close)
	received := make(chan LedgerAction, 1)
	sub, err := operator.Subscribe(bus.LedgerActionSubject("member-a"), func(msg *nats.Msg) {
		var a LedgerAction
		if err := json.Unmarshal(msg.Data, &a); err == nil {
			received <- a
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := operator.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	recorder, err := bus.Connect(s.ClientURL(), bus.Config{Username: "recorder", Password: "rec_pass"})
	if err != nil {
		t.Fatalf("connect as recorder: %v", err)
	}
	t.Cleanup(recorder.Close)
	if err := PublishLedgerAction(recorder, LedgerAction{
		Kind:       LedgerActionWakeCell,
		Cluster:    "member-a",
		Namespace:  "team-b",
		Controller: "cell-b",
		Actor:      LedgerActionRecorder,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case a := <-received:
		if a.Cluster != "member-a" || a.Kind != LedgerActionWakeCell {
			t.Fatalf("got %+v, want a member-a wake_cell", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the member-a operator received no ledger action")
	}

	nc, lastErr := connectRecorderRaw(t, s)
	if _, err := nc.Subscribe(bus.LedgerActionSubject("member-a"), func(*nats.Msg) {}); err != nil {
		t.Fatalf("subscribe call itself must succeed (async denial): %v", err)
	}
	_ = nc.Flush()
	waitPermissionViolation(t, lastErr, "subscribe "+bus.LedgerActionSubject("member-a"))
}

// TestPublishLedgerActionRefusesEmptyCluster asserts an action with no
// cluster never reaches the bus. Its subject names no operator.
func TestPublishLedgerActionRefusesEmptyCluster(t *testing.T) {
	s := startLedgerActionTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	received := make(chan struct{}, 1)
	sub, err := conn.Subscribe("varroa.ledger.actions.>", func(*nats.Msg) { received <- struct{}{} })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	err = PublishLedgerAction(conn, LedgerAction{Kind: LedgerActionWakeCell, Namespace: "team-a", Controller: "cell-1"})
	if err == nil {
		t.Fatal("want an error for an action with an empty cluster")
	}
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	select {
	case <-received:
		t.Fatal("an action with an empty cluster reached the bus")
	case <-time.After(200 * time.Millisecond):
	}
}
