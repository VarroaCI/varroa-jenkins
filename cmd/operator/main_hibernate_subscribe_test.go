package main

import (
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

// TestSubscribeHibernationWake_RecoversAfterDeniedSubscribe starts the
// wake subscriber while NATS denies its subject. The subscribe returns no
// error, and wakes reach the operator after the reload adds the grant.
func TestSubscribeHibernationWake_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := bustest.StartDenyServer(t, bus.WakeSubjectWildcard("core"))
	conn := denyConn(t, d, bustest.RestrictedUser, bustest.RestrictedPassword)
	pub := denyConn(t, d, bustest.ClientUser, bustest.ClientPassword)

	var got atomic.Int64
	var wrong atomic.Int64
	l, err := subscribeHibernationWake(conn, "core", "operator-workers", func(ns, name string) {
		if ns != "team-a" || name != "jenkins" {
			wrong.Add(1)
		}
		got.Add(1)
	}, slog.Default())
	if err != nil {
		t.Fatalf("a denied subscribe must not return an error: %v", err)
	}
	defer func() { _ = l.Unsubscribe() }()
	subject := bus.WakeSubject("core", "team-a", "jenkins")
	if publishReaches(t, pub, subject, nil, &got) {
		t.Fatal("a denied wake subscriber received a wake")
	}

	d.SetDeny()
	bustest.Eventually(t, "the wake subscriber did not recover after the reload", func() bool {
		return publishReaches(t, pub, subject, nil, &got)
	})
	if n := wrong.Load(); n != 0 {
		t.Fatalf("%d wakes named the wrong controller", n)
	}
}
