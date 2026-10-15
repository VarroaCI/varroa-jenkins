package api

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

// tailRecoveryConn connects to d as user. The test cleanup closes the
// connection.
func tailRecoveryConn(t *testing.T, d *bustest.DenyServer, user, pass string) *bus.Conn {
	t.Helper()
	c, err := bus.Connect(d.URL(), bus.Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("bus connect as %s: %v", user, err)
	}
	t.Cleanup(c.Close)
	return c
}

// tailChunkArrives publishes one chunk on subject and reports whether ch
// receives it within 200 ms. It drains older messages first.
func tailChunkArrives(t *testing.T, pub *bus.Conn, ch <-chan []byte, subject string) bool {
	t.Helper()
	for {
		select {
		case <-ch:
			continue
		default:
		}
		break
	}
	payload := []byte(`{"offset":0,"len":1,"data":"eA=="}`)
	if err := pub.Publish(subject, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case got, ok := <-ch:
		return ok && string(got) == string(payload)
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// TestRunTailRegistry_RecoversAfterDeniedSubscribe attaches a tail while
// NATS denies the log subject, as before an authorization reload reaches
// the server. The attach succeeds, and chunks reach the viewer after the
// reload adds the grant, without a new attach.
func TestRunTailRegistry_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := bustest.StartDenyServer(t, "logs.>")
	conn := tailRecoveryConn(t, d, bustest.RestrictedUser, bustest.RestrictedPassword)
	pub := tailRecoveryConn(t, d, bustest.ClientUser, bustest.ClientPassword)

	const cluster, ns, name, runID = "core", "ns1", "cell1", "run-tail-denied"
	gw := &fakeTailGateway{}
	gw.subscribe(t, pub, ns, name)

	reg := newRunTailRegistry(conn)
	ch, release, err := reg.attach(cluster, ns, name, runID, "folder/job", 7)
	if err != nil {
		t.Fatalf("attach while denied: %v", err)
	}
	t.Cleanup(release)

	subject := bus.LogSubject(cluster, ns, name, runID)
	if tailChunkArrives(t, pub, ch, subject) {
		t.Fatal("a denied tail delivered a chunk")
	}

	d.SetDeny()
	bustest.Eventually(t, "the tail did not recover after the reload", func() bool {
		return tailChunkArrives(t, pub, ch, subject)
	})
}

// TestRunTailRegistry_RecoversAfterGrantRemovedAndRestored removes the
// log grant from a live tail and restores it. The lease keeps renewing
// while the grant is absent, and chunks reach the viewer again after the
// restore. A release during the outage sends exactly one stop.
func TestRunTailRegistry_RecoversAfterGrantRemovedAndRestored(t *testing.T) {
	d := bustest.StartDenyServer(t)
	conn := tailRecoveryConn(t, d, bustest.RestrictedUser, bustest.RestrictedPassword)
	pub := tailRecoveryConn(t, d, bustest.ClientUser, bustest.ClientPassword)

	const cluster, ns, name, runID = "core", "ns1", "cell1", "run-tail-reload"
	gw := &fakeTailGateway{}
	gw.subscribe(t, pub, ns, name)

	reg := newRunTailRegistry(conn)
	reg.renewOverride = 30 * time.Millisecond

	ch, release, err := reg.attach(cluster, ns, name, runID, "folder/job", 7)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	subject := bus.LogSubject(cluster, ns, name, runID)
	bustest.Eventually(t, "the tail did not deliver before the reload", func() bool {
		return tailChunkArrives(t, pub, ch, subject)
	})

	d.SetDeny("logs.>")
	bustest.Eventually(t, "the tail still delivered after the grant was removed", func() bool {
		return !tailChunkArrives(t, pub, ch, subject)
	})
	startsWhileDenied := atomic.LoadInt32(&gw.starts)
	waitForCondition(t, func() bool { return atomic.LoadInt32(&gw.starts) > startsWhileDenied }, "the lease stopped renewing while the grant was absent")

	d.SetDeny()
	bustest.Eventually(t, "the tail did not recover after the grant was restored", func() bool {
		return tailChunkArrives(t, pub, ch, subject)
	})

	d.SetDeny("logs.>")
	release()
	released = true
	waitForCondition(t, func() bool { return atomic.LoadInt32(&gw.stops) == 1 }, "expected exactly one stop request after the release")
	waitForCondition(t, func() bool {
		for {
			select {
			case _, open := <-ch:
				if !open {
					return true
				}
			default:
				return false
			}
		}
	}, "the viewer channel stayed open after the release")
	reg.mu.Lock()
	remaining := len(reg.entries)
	reg.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected the entry to be removed, got %d remaining", remaining)
	}
	startsAtRelease := atomic.LoadInt32(&gw.starts)
	time.Sleep(100 * time.Millisecond) // fg-ok: three renewal periods
	if got := atomic.LoadInt32(&gw.starts); got != startsAtRelease {
		t.Fatalf("the lease renewed after the release: %d starts, want %d", got, startsAtRelease)
	}
	if got := atomic.LoadInt32(&gw.stops); got != 1 {
		t.Fatalf("expected one stop request, got %d", got)
	}
}
