package bus

import (
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startAgentRunnerACLServer starts an embedded nats-server with the agentrunner
// and gateway grants that the chart renders, plus an admin user that creates the
// activity stream. The grants match charts/varroa/templates/nats-auth-config.yaml.
func startAgentRunnerACLServer(t *testing.T) *server.Server {
	t.Helper()
	perms := func(pub, sub []string) *server.Permissions {
		return &server.Permissions{
			Publish:   &server.SubjectPermission{Allow: pub},
			Subscribe: &server.SubjectPermission{Allow: sub},
		}
	}
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		Users: []*server.User{
			{
				Username:    "admin",
				Password:    "admin_pass",
				Permissions: perms([]string{">"}, []string{">"}),
			},
			{
				Username: "agentrunner",
				Password: "ar_pass",
				Permissions: perms(
					[]string{
						"agentsessions.>",
						"operator.*.forge.read",
						"operator.*.forge.issue",
						"varroa.recordstore.notes.post",
						"$JS.API.STREAM.INFO.varroa_activity",
						"$JS.API.CONSUMER.CREATE.varroa_activity.>",
						"$JS.API.CONSUMER.DURABLE.CREATE.varroa_activity.>",
						"$JS.API.CONSUMER.INFO.varroa_activity.>",
						"$JS.API.CONSUMER.MSG.NEXT.varroa_activity.>",
						"$JS.ACK.varroa_activity.>",
					},
					[]string{"varroa.assistant.control", "_INBOX_agentrunner.>"},
				),
			},
			{
				Username: "gateway",
				Password: "gw_pass",
				Permissions: perms(
					[]string{"mite.*.*.*.in", "builds.>", "varroa.ledger.claim", "_INBOX_gateway.>"},
					[]string{"mite.*.*.*.desired", "_INBOX_gateway.>"},
				),
			},
		},
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// connectAgentACL connects one user and closes the connection at the test end.
func connectAgentACL(t *testing.T, url, user, pass, inbox string) *Conn {
	t.Helper()
	conn, err := Connect(url, Config{Username: user, Password: pass, InboxPrefix: inbox})
	if err != nil {
		t.Fatalf("connect %s: %v", user, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// asyncDenial connects as user and returns a function that reports the first
// permissions violation. nats.go delivers a violation through the error handler
// after the server rejects the operation.
func asyncDenial(t *testing.T, url, user, pass string) (*nats.Conn, func() error) {
	t.Helper()
	var mu sync.Mutex
	var first error
	nc, err := nats.Connect(url, nats.UserInfo(user, pass), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
		}
	}))
	if err != nil {
		t.Fatalf("connect %s: %v", user, err)
	}
	t.Cleanup(nc.Close)
	return nc, func() error {
		mu.Lock()
		defer mu.Unlock()
		return first
	}
}

// agentTriggersConsumer and agentTriggersConfig mirror the durable consumer that
// internal/tenantagent/triggers creates. The bus package cannot import that package.
const agentTriggersConsumer = "agent-triggers"

func agentTriggersConfig() *nats.ConsumerConfig {
	return &nats.ConsumerConfig{
		Durable:       agentTriggersConsumer,
		FilterSubject: "activity.>",
		DeliverPolicy: nats.DeliverNewPolicy,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       60 * time.Second,
		MaxDeliver:    -1,
		MaxAckPending: 256,
	}
}

// TestACL_AgentRunner proves that the agentrunner user reads its own durable
// consumer on the activity stream, and that it cannot publish on activity.* or
// create the stream.
func TestACL_AgentRunner(t *testing.T) {
	srv := startAgentRunnerACLServer(t)
	url := srv.ClientURL()

	admin := connectAgentACL(t, url, "admin", "admin_pass", "_INBOX_admin")
	js, err := admin.NATSConn().JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "varroa_activity", Subjects: []string{"activity.>"}}); err != nil {
		t.Fatalf("admin creates the activity stream: %v", err)
	}

	ar := connectAgentACL(t, url, "agentrunner", "ar_pass", "_INBOX_agentrunner")
	t.Run("consumer create, fetch, and ack", func(t *testing.T) {
		if err := ar.EnsureConsumer("varroa_activity", agentTriggersConsumer, agentTriggersConfig()); err != nil {
			t.Fatalf("EnsureConsumer: %v", err)
		}
		// The consumer starts at DeliverNew, so the row is published after it.
		if _, err := js.Publish("activity.build.failed", []byte(`{"type":"build.failed"}`)); err != nil {
			t.Fatalf("admin publishes an activity row: %v", err)
		}
		sub, err := ar.PullSubscribe("activity.>", "varroa_activity", agentTriggersConsumer)
		if err != nil {
			t.Fatalf("PullSubscribe: %v", err)
		}
		msgs, err := sub.Fetch(1, nats.MaxWait(5*time.Second))
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Fetch = %d, %v; want one message", len(msgs), err)
		}
		if err := msgs[0].Ack(); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	})

	t.Run("publish on activity is denied", func(t *testing.T) {
		nc, denied := asyncDenial(t, url, "agentrunner", "ar_pass")
		if err := nc.Publish("activity.forged", []byte("x")); err != nil {
			t.Fatalf("Publish itself returns nil on the async path: %v", err)
		}
		nc.Flush()
		time.Sleep(200 * time.Millisecond)
		if denied() == nil {
			t.Fatal("publish on activity.forged was not denied")
		}
	})

	t.Run("stream create is denied", func(t *testing.T) {
		nc, _ := asyncDenial(t, url, "agentrunner", "ar_pass")
		njs, err := nc.JetStream(nats.MaxWait(2 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := njs.AddStream(&nats.StreamConfig{Name: "varroa_activity_forged", Subjects: []string{"forged.>"}}); err == nil {
			t.Fatal("agentrunner created a stream")
		}
	})
}

// TestACL_GatewayCannotPublishForgeIssue proves the gateway holds no grant on
// the forge issue verb. The issue verb belongs to the BFF and the agent runner.
func TestACL_GatewayCannotPublishForgeIssue(t *testing.T) {
	srv := startAgentRunnerACLServer(t)
	nc, denied := asyncDenial(t, srv.ClientURL(), "gateway", "gw_pass")
	if err := nc.Publish("operator.core.forge.issue", []byte("{}")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	nc.Flush()
	time.Sleep(200 * time.Millisecond)
	if denied() == nil {
		t.Fatal("gateway published operator.core.forge.issue")
	}
}
