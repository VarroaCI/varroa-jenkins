package store

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

type recordedPub struct {
	subjects []string
	bodies   [][]byte
}

func (r *recordedPub) publish(subject string, data []byte) error {
	r.subjects = append(r.subjects, subject)
	r.bodies = append(r.bodies, data)
	return nil
}

// TestNotify_ContentFree proves a write publishes exactly four keys, with no
// transcript text, on the session subject.
func TestNotify_ContentFree(t *testing.T) {
	ctx := context.Background()
	inner := NewMemory(Options{Now: time.Now})
	pub := &recordedPub{}
	st := WithNotify(inner, pub.publish, slog.New(slog.NewTextHandler(io.Discard, nil)))

	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secret := "PRIVATE-TRANSCRIPT-TEXT"
	user, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: secret})
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "u", Cluster: "core", Tenant: "",
		Trigger: agent.TriggerManual, Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateRunning, LeaseOwner: "owner-1",
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{user}); err != nil {
		t.Fatal(err)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "agentsessions.core._none" {
		t.Fatalf("subjects = %v, want [agentsessions.core._none]", pub.subjects)
	}
	var body map[string]any
	if err := json.Unmarshal(pub.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 {
		t.Errorf("notice has %d keys, want 4: %v", len(body), body)
	}
	for _, k := range []string{"session_id", "kind", "state", "seq"} {
		if _, ok := body[k]; !ok {
			t.Errorf("notice lacks %q", k)
		}
	}
	if strings.Contains(string(pub.bodies[0]), secret) {
		t.Errorf("notice carries transcript text: %s", pub.bodies[0])
	}
}

// sweepNotify builds a notifying store over a memory store on a test clock.
func sweepNotify(t *testing.T) (Store, *testClock, *recordedPub, Store) {
	t.Helper()
	clock := newTestClock()
	inner := NewMemory(Options{Now: clock.Now})
	pub := &recordedPub{}
	return WithNotify(inner, pub.publish, slog.New(slog.NewTextHandler(io.Discard, nil))), clock, pub, inner
}

// decodeNotice reads one notice body.
func decodeNotice(t *testing.T, body []byte) notice {
	t.Helper()
	var n notice
	if err := json.Unmarshal(body, &n); err != nil {
		t.Fatalf("decode notice %s: %v", body, err)
	}
	return n
}

// lastEventSeq returns the sequence number of the last event of a session.
func lastEventSeq(t *testing.T, inner Store, id uuid.UUID) int {
	t.Helper()
	evs, err := inner.Events(context.Background(), id, 0, 1000)
	if err != nil || len(evs) == 0 {
		t.Fatalf("Events: %v (%d events)", err, len(evs))
	}
	return evs[len(evs)-1].Seq
}

func TestNotify_ExpireDuePublishes(t *testing.T) {
	st, clock, pub, inner := sweepNotify(t)
	ctx := context.Background()
	sess := startRunning(t, st)
	park(t, st, clock, sess, time.Minute)
	pub.subjects, pub.bodies = nil, nil
	clock.Advance(2 * time.Minute)
	if _, err := st.ExpireDue(ctx); err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "agentsessions.core.acme" {
		t.Fatalf("subjects = %v, want [agentsessions.core.acme]", pub.subjects)
	}
	n := decodeNotice(t, pub.bodies[0])
	if n.State != string(agent.StateExpired) || n.SessionID != sess.ID.String() {
		t.Fatalf("notice = %+v, want expired for %s", n, sess.ID)
	}
	if want := lastEventSeq(t, inner, sess.ID); n.Seq != want {
		t.Fatalf("seq = %d, want %d", n.Seq, want)
	}
}

func TestNotify_SweepLeasesPublishes(t *testing.T) {
	st, clock, pub, inner := sweepNotify(t)
	ctx := context.Background()
	sess := startRunning(t, st)
	pub.subjects, pub.bodies = nil, nil
	clock.Advance(LeaseTTL + time.Second)
	if _, err := st.SweepLeases(ctx); err != nil {
		t.Fatalf("SweepLeases: %v", err)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "agentsessions.core.acme" {
		t.Fatalf("subjects = %v, want [agentsessions.core.acme]", pub.subjects)
	}
	n := decodeNotice(t, pub.bodies[0])
	if n.State != string(agent.StateFailed) || n.SessionID != sess.ID.String() {
		t.Fatalf("notice = %+v, want failed for %s", n, sess.ID)
	}
	if want := lastEventSeq(t, inner, sess.ID); n.Seq != want {
		t.Fatalf("seq = %d, want %d", n.Seq, want)
	}
}

// TestNotify_SweepRoutesThroughWrapper guards the embedding trap: Sweep must
// reach the wrapper's ExpireDue and SweepLeases, or no notice is published.
func TestNotify_SweepRoutesThroughWrapper(t *testing.T) {
	st, clock, pub, _ := sweepNotify(t)
	ctx := context.Background()
	parked := startRunning(t, st)
	park(t, st, clock, parked, time.Minute)
	leased := startRunning(t, st)
	pub.subjects, pub.bodies = nil, nil
	clock.Advance(2 * time.Minute)
	expired, lost, err := st.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(expired) != 1 || len(lost) != 1 {
		t.Fatalf("Sweep = expired %d, lost %d; want 1 and 1", len(expired), len(lost))
	}
	if len(pub.bodies) != 2 {
		t.Fatalf("publishes = %d, want 2", len(pub.bodies))
	}
	gotStates := map[string]string{}
	for _, b := range pub.bodies {
		n := decodeNotice(t, b)
		gotStates[n.SessionID] = n.State
	}
	if gotStates[parked.ID.String()] != string(agent.StateExpired) {
		t.Errorf("parked session notice = %q, want expired", gotStates[parked.ID.String()])
	}
	if gotStates[leased.ID.String()] != string(agent.StateFailed) {
		t.Errorf("leased session notice = %q, want failed", gotStates[leased.ID.String()])
	}
}

func TestNotify_SweepNoChangeNoNotice(t *testing.T) {
	st, _, pub, _ := sweepNotify(t)
	startRunning(t, st)
	pub.subjects, pub.bodies = nil, nil
	if _, _, err := st.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(pub.bodies) != 0 {
		t.Fatalf("publishes = %d, want 0", len(pub.bodies))
	}
}
