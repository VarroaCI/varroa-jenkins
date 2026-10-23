package sse

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

// fanoutConn connects to d as user and closes the connection at cleanup.
func fanoutConn(t *testing.T, d *bustest.DenyServer, user, pass string) *bus.Conn {
	t.Helper()
	c, err := bus.Connect(d.URL(), bus.Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect %s: %v", user, err)
	}
	t.Cleanup(c.Close)
	return c
}

// recordArrives publishes one message on subject and reports whether a
// record with event arrives on ch within 200 ms. It discards every other
// record.
func recordArrives(t *testing.T, pub *bus.Conn, ch <-chan Record, subject string, data []byte, event string) bool {
	t.Helper()
	if err := pub.Publish(subject, data); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case rec := <-ch:
			if rec.Event == event {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

// startFanout runs start in the background and stops it at cleanup.
func startFanout(t *testing.T, start func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := start(ctx); err != nil {
			t.Errorf("fanout start: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestBusFanout_RecoversAfterDeniedSubscribe starts the BFF fan-out while
// NATS denies both of its subjects, as before an authorization reload
// reaches the server. Brood and activity events reach an SSE subscriber
// after the reload adds the grants, without a restart.
func TestBusFanout_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := bustest.StartDenyServer(t, bus.BroodWildcard, bus.ActivityWildcard)
	conn := fanoutConn(t, d, bustest.RestrictedUser, bustest.RestrictedPassword)
	pub := fanoutConn(t, d, bustest.ClientUser, bustest.ClientPassword)

	bf := NewBusFanout(conn)
	ch := bf.Subscribe("core/ns/c1")
	startFanout(t, bf.Start)

	broodSubject := bus.BroodSubject("core")
	brood := []byte(`{"event":"connected","name":"c1","namespace":"ns","cluster":"core"}`)
	activitySubject := bus.ActivitySubject("core", "ns", "c1")
	act := []byte(`{"type":"controller.created"}`)
	if recordArrives(t, pub, ch, broodSubject, brood, "connected") {
		t.Fatal("a denied fan-out delivered a brood event")
	}
	if recordArrives(t, pub, ch, activitySubject, act, "controller.created") {
		t.Fatal("a denied fan-out delivered an activity event")
	}

	d.SetDeny()
	bustest.Eventually(t, "brood events did not recover after the reload", func() bool {
		return recordArrives(t, pub, ch, broodSubject, brood, "connected")
	})
	bustest.Eventually(t, "activity events did not recover after the reload", func() bool {
		return recordArrives(t, pub, ch, activitySubject, act, "controller.created")
	})
}

// TestRunFanout_RecoversAfterDeniedSubscribe starts the run fan-out while
// NATS denies its subject. Run notifications reach a tenant subscriber after
// the reload adds the grant, without a restart.
func TestRunFanout_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := bustest.StartDenyServer(t, bus.RunNotifyWildcard)
	conn := fanoutConn(t, d, bustest.RestrictedUser, bustest.RestrictedPassword)
	pub := fanoutConn(t, d, bustest.ClientUser, bustest.ClientPassword)

	rf := mustNewRunFanout(t, conn, noop.NewMeterProvider())
	ch := rf.Subscribe([]string{"core/team-a"}, nil)
	startFanout(t, rf.Start)

	subject := bus.RunNotifySubject("core", "team-a")
	note := runNotifyJSON("run", "core", "team-a", "app/main")
	if recordArrives(t, pub, ch, subject, note, "run") {
		t.Fatal("a denied run fan-out delivered a notification")
	}

	d.SetDeny()
	bustest.Eventually(t, "run notifications did not recover after the reload", func() bool {
		return recordArrives(t, pub, ch, subject, note, "run")
	})
}
