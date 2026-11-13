package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestKVCreateAtomic(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("create_test", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	if err := kv.Create("k", []byte("v1")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err = kv.Create("k", []byte("v2"))
	if !errors.Is(err, ErrKVKeyExists) {
		t.Fatalf("second Create: want ErrKVKeyExists, got %v", err)
	}
	if err := kv.Create("k2", []byte("v")); err != nil {
		t.Fatalf("Create distinct key: %v", err)
	}

	got, err := kv.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("value overwritten by failed Create: got %q", got)
	}
}

func TestKVGetFromLeaderReadsCommittedValue(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("leader_read", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	expect := func(step string, want string) {
		t.Helper()
		got, err := kv.GetFromLeader("k")
		if err != nil {
			t.Fatalf("%s: GetFromLeader: %v", step, err)
		}
		if want == "" {
			if got != nil {
				t.Fatalf("%s: want nil, got %q", step, got)
			}
			return
		}
		if string(got) != want {
			t.Fatalf("%s: want %q, got %q", step, want, got)
		}
	}

	expect("absent key", "")
	if err := kv.PutString("k", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	expect("after put", "v1")
	if err := kv.Delete("k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	expect("after delete", "")
	if err := kv.PutString("k", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	expect("after re-put", "v2")
	if err := kv.KeyValue().Purge("k"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	expect("after purge", "")
}

func TestKVGetFromLeaderSkipsDirectGet(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("leader_route", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}
	if err := kv.PutString("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}

	mon, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("monitor connect: %v", err)
	}
	t.Cleanup(mon.Close)
	direct, err := mon.SubscribeSync("$JS.API.DIRECT.GET.KV_leader_route.>")
	if err != nil {
		t.Fatalf("subscribe direct: %v", err)
	}
	leader, err := mon.SubscribeSync("$JS.API.STREAM.MSG.GET.KV_leader_route")
	if err != nil {
		t.Fatalf("subscribe leader: %v", err)
	}
	if err := mon.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := kv.GetFromLeader("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("GetFromLeader = %q, %v; want v", got, err)
	}
	if _, err := leader.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("no leader request seen: %v", err)
	}
	if _, err := direct.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatal("GetFromLeader sent a direct get")
	}

	// Control: the ordinary Get uses the direct path.
	if _, err := kv.Get("k"); err != nil {
		t.Fatalf("control Get: %v", err)
	}
	if _, err := direct.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("control Get sent no direct get: %v", err)
	}
}

func TestKVGetFromLeaderNoLeaderIsAnError(t *testing.T) {
	// A server without JetStream gives no stream leader for the read.
	srv, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(srv.Shutdown)
	c, err := Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)

	kv := &KV{
		js:            c.JetStream(),
		stream:        "KV_presence_test",
		subjectPrefix: "$KV.presence_test.",
	}
	const subject = "$JS.API.STREAM.MSG.GET.KV_presence_test"
	respond := func(t *testing.T, body string) {
		t.Helper()
		nc, err := nats.Connect(srv.ClientURL())
		if err != nil {
			t.Fatalf("responder connect: %v", err)
		}
		t.Cleanup(nc.Close)
		if _, err := nc.Subscribe(subject, func(m *nats.Msg) {
			_ = m.Respond([]byte(body))
		}); err != nil {
			t.Fatalf("subscribe responder: %v", err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatalf("flush responder: %v", err)
		}
	}

	t.Run("cluster unavailable", func(t *testing.T) {
		respond(t, `{"type":"io.nats.jetstream.api.v1.stream_msg_get_response","error":{"code":503,"err_code":10008,"description":"JetStream system temporarily unavailable"}}`)
		got, err := kv.GetFromLeader("k")
		if got != nil || err == nil || errors.Is(err, nats.ErrMsgNotFound) {
			t.Fatalf("got %q, %v; want nil and a non-missing error", got, err)
		}
	})

	t.Run("no responder", func(t *testing.T) {
		// A fresh server with no responder on the subject.
		srv2, err := server.NewServer(&server.Options{Port: -1})
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		srv2.Start()
		if !srv2.ReadyForConnections(5 * time.Second) {
			t.Fatal("server not ready")
		}
		t.Cleanup(srv2.Shutdown)
		c2, err := Connect(srv2.ClientURL())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(c2.Close)
		kv2 := &KV{js: c2.JetStream(), stream: "KV_presence_test", subjectPrefix: "$KV.presence_test."}
		got, err := kv2.GetFromLeader("k")
		if got != nil || err == nil || errors.Is(err, nats.ErrMsgNotFound) {
			t.Fatalf("got %q, %v; want nil and a non-missing error", got, err)
		}
	})

	t.Run("message not found is a miss", func(t *testing.T) {
		respond(t, `{"type":"io.nats.jetstream.api.v1.stream_msg_get_response","error":{"code":404,"err_code":10037,"description":"no message found"}}`)
		got, err := kv.GetFromLeader("k")
		if got != nil || err != nil {
			t.Fatalf("got %q, %v; want nil and nil", got, err)
		}
	})
}
