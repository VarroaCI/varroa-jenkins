package transport

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestBusTransport_ForgetDropsTheInboundSubscription pins that Forget ends
// the NATS subscription and removes the subscription, result, and report
// entries of the mite.
func TestBusTransport_ForgetDropsTheInboundSubscription(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	ns, name := "ns", "gone"
	key := ns + "/" + name
	base := tr.conn.NATSConn().NumSubscriptions()

	tr.DrainResults(ns, name)
	tr.mu.Lock()
	tr.reports[key] = &mitev1.ObservabilityReport{}
	tr.mu.Unlock()
	if n := tr.conn.NATSConn().NumSubscriptions() - base; n != 1 {
		t.Fatalf("new NATS subscriptions = %d, want 1", n)
	}

	tr.Forget(ns, name)

	if n := tr.conn.NATSConn().NumSubscriptions() - base; n != 0 {
		t.Fatalf("NATS subscriptions after Forget = %d, want 0", n)
	}
	tr.mu.Lock()
	_, sub := tr.subs[key]
	_, res := tr.results[key]
	_, rep := tr.reports[key]
	tr.mu.Unlock()
	if sub || res || rep {
		t.Fatalf("entries after Forget: subscription %v, results %v, report %v; want none", sub, res, rep)
	}
}

// TestBusTransport_ForgetThenEnsureSubStartsFresh pins that a new
// controller of the same name drains no result of the old mite.
func TestBusTransport_ForgetThenEnsureSubStartsFresh(t *testing.T) {
	s := startNATS(t)
	tr := newBusTransport(t, s)
	ns, name := "ns", "again"
	subject := bus.MiteInSubject(bus.DefaultCluster, ns, name)

	gw, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("gateway connect: %v", err)
	}
	t.Cleanup(gw.Close)

	tr.DrainResults(ns, name)
	settle(t, tr.conn)
	if err := gw.Publish(subject, commandResult(t, "old")); err != nil {
		t.Fatalf("publish old result: %v", err)
	}
	settle(t, gw)
	tr.mu.Lock()
	buffered := len(tr.results[ns+"/"+name])
	tr.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("buffered results before Forget = %d, want 1", buffered)
	}

	tr.Forget(ns, name)
	if got := tr.DrainResults(ns, name); len(got) != 0 {
		t.Fatalf("the new controller drained %d results of the old mite, want 0", len(got))
	}

	settle(t, tr.conn)
	if err := gw.Publish(subject, commandResult(t, "new")); err != nil {
		t.Fatalf("publish new result: %v", err)
	}
	var got []*mitev1.CommandResult
	waitUntil(t, func() bool {
		got = append(got, tr.DrainResults(ns, name)...)
		return len(got) > 0
	}, "the new subscription delivered no command result")
	if len(got) != 1 || got[0].CommandId != "new" {
		t.Fatalf("results = %v, want only the new one", got)
	}
}

// TestBusTransport_ForgetDropsALateReport pins that a report callback that
// runs after Forget does not put the report of the old mite back.
func TestBusTransport_ForgetDropsALateReport(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	ns, name := "ns", "late"
	key := ns + "/" + name

	tr.DrainResults(ns, name)
	tr.mu.Lock()
	old := tr.subs[key]
	tr.mu.Unlock()

	tr.Forget(ns, name)
	tr.storeReport(key, old, &mitev1.ObservabilityReport{})
	tr.mu.Lock()
	_, rep := tr.reports[key]
	tr.mu.Unlock()
	if rep {
		t.Fatal("a callback of the forgotten subscription stored a report")
	}

	tr.DrainResults(ns, name)
	tr.mu.Lock()
	cur := tr.subs[key]
	tr.mu.Unlock()
	tr.storeReport(key, cur, &mitev1.ObservabilityReport{})
	tr.mu.Lock()
	_, rep = tr.reports[key]
	tr.mu.Unlock()
	if !rep {
		t.Fatal("a callback of the current subscription stored no report")
	}
}

// TestBusTransport_ForgetUnknownKeyIsANoOp pins that Forget of a mite with
// no entry changes nothing and does not panic.
func TestBusTransport_ForgetUnknownKeyIsANoOp(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	tr.DrainResults("ns", "kept")
	tr.Forget("ns", "never")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.subs) != 1 || len(tr.results) != 1 {
		t.Fatalf("entries = %d subscriptions, %d result channels; want 1 and 1", len(tr.subs), len(tr.results))
	}
}

// TestBusTransport_CloseClearsReports pins that Close drops the cached
// reports with the subscriptions.
func TestBusTransport_CloseClearsReports(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	tr.DrainResults("ns", "a")
	data, err := json.Marshal(&mitev1.ObservabilityReport{})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := tr.snapshotKV.Put(bus.ObservabilityKey(bus.DefaultCluster, "ns", "a"), data); err != nil {
		t.Fatalf("put report: %v", err)
	}
	if tr.ObservabilityReport("ns", "a") == nil {
		t.Fatal("no report before Close")
	}
	tr.Close()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.reports) != 0 {
		t.Fatalf("reports after Close = %d, want 0", len(tr.reports))
	}
}
