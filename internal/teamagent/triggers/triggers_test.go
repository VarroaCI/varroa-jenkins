package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

var testNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// testTA returns an enabled TeamAgent named triage in namespace team-a that
// serves team obj and fires on the given triggers.
func testTA(triggers ...string) *v1alpha1.TeamAgent {
	ta := &v1alpha1.TeamAgent{}
	ta.Name = "triage"
	ta.Namespace = "team-a"
	ta.UID = "uid-triage"
	ta.Spec.TeamRef.Name = "obj"
	for _, tr := range triggers {
		ta.Spec.Triggers = append(ta.Spec.Triggers, v1alpha1.TeamAgentTrigger{Type: tr})
	}
	return ta
}

func testMatcher(ta *v1alpha1.TeamAgent, cluster string) *Matcher {
	in := accept.Input{TeamAgents: []v1alpha1.TeamAgent{*ta}, ReleaseNamespace: "team-a"}
	return &Matcher{
		Accepted:  func() accept.Input { return in },
		Placement: func(string) (string, bool) { return cluster, true },
	}
}

func rowOf(typ, itemPath, cluster string) activity.Event {
	return activity.Event{Type: typ, ItemPath: itemPath, Cluster: cluster, Timestamp: testNow}
}

func TestMatcher(t *testing.T) {
	ta := testTA("run.failed", "manual")
	m := testMatcher(ta, "core")
	cases := []struct {
		name string
		e    activity.Event
		want Outcome
	}{
		{"failed build on a job", rowOf("build.failed", "obj/build", "core"), OutcomeCreate},
		{"unknown type is dropped", rowOf("job.renamed", "obj/build", "core"), OutcomeAck},
		{"no item path is dropped", rowOf("build.failed", "", "core"), OutcomeAck},
		{"another cluster is dropped", rowOf("build.failed", "obj/build", "member-a"), OutcomeAck},
		{"a trigger the agent does not enable is dropped", rowOf("build.interrupted", "obj/build", "core"), OutcomeAck},
		{"an unknown tenant is dropped", rowOf("build.failed", "other/build", "core"), OutcomeAck},
		{"an old row is dropped", func() activity.Event {
			e := rowOf("build.failed", "obj/build", "core")
			e.Timestamp = testNow.Add(-2 * time.Hour)
			return e
		}(), OutcomeAck},
		{"manual names this agent", func() activity.Event {
			e := rowOf("teamagent.run_requested", "obj", "core")
			e.Reason = "team-a/triage"
			return e
		}(), OutcomeCreate},
		{"manual names another agent", func() activity.Event {
			e := rowOf("teamagent.run_requested", "obj", "core")
			e.Reason = "team-a/other"
			return e
		}(), OutcomeAck},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outcome := m.Match(c.e, testNow)
			if outcome != c.want {
				t.Fatalf("outcome = %v, want %v", outcome, c.want)
			}
			if outcome == OutcomeCreate && (got.Tenant != "obj" || got.TA == nil) {
				t.Fatalf("match = %+v", got)
			}
		})
	}
}

func TestMatcher_ItemKey(t *testing.T) {
	m := testMatcher(testTA("run.failed"), "core")
	got, _ := m.Match(rowOf("build.failed", "obj/build", "core"), testNow)
	if got.ItemKey != "obj/build" {
		t.Errorf("job item key = %q", got.ItemKey)
	}
	got, _ = m.Match(rowOf("build.failed", "obj", "core"), testNow)
	if got.ItemKey != "obj" {
		t.Errorf("tenant item key = %q", got.ItemKey)
	}
}

// fakePool counts reservations and starts.
type fakePool struct {
	reserve bool
	held    int
	started []agent.Session
}

func (p *fakePool) TryReserve() bool {
	if !p.reserve {
		return false
	}
	p.held++
	return true
}

func (p *fakePool) Release() { p.held-- }

func (p *fakePool) Start(s agent.Session, _ loop.Trigger) { p.started = append(p.started, s) }

// fakeCreator returns one fixed outcome.
type fakeCreator struct {
	outcome CreateOutcome
	err     error
	calls   int
}

func (c *fakeCreator) Create(context.Context, Match) (agent.Session, CreateOutcome, error) {
	c.calls++
	return agent.Session{ID: [16]byte{1}, State: agent.StateRunning, Kind: agent.KindTeamAgent}, c.outcome, c.err
}

// fakeMsg records the ack and the delay of a nak.
type fakeMsg struct {
	acked bool
	nak   time.Duration
	naked bool
}

func (m *fakeMsg) Ack() error { m.acked = true; return nil }

func (m *fakeMsg) NakWithDelay(d time.Duration) error { m.naked = true; m.nak = d; return nil }

func handoffFor(pool *fakePool, creator *fakeCreator) *Handoff {
	return &Handoff{
		Matcher: testMatcher(testTA("run.failed"), "core"),
		Creator: creator,
		Pool:    pool,
		Cluster: "core",
		Now:     func() time.Time { return testNow },
	}
}

func mustJSON(t *testing.T, e activity.Event) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandoff_PoolFullNaksAfter60s(t *testing.T) {
	pool := &fakePool{reserve: false}
	creator := &fakeCreator{outcome: Created}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("build.failed", "obj/build", "core")))
	if !msg.naked || msg.nak != 60*time.Second || creator.calls != 0 {
		t.Fatalf("pool full: naked=%v delay=%v calls=%d", msg.naked, msg.nak, creator.calls)
	}
}

func TestHandoff_CreateErrorNaksAfter60s(t *testing.T) {
	pool := &fakePool{reserve: true}
	creator := &fakeCreator{err: errors.New("store down")}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("build.failed", "obj/build", "core")))
	if !msg.naked || msg.nak != 60*time.Second || pool.held != 0 {
		t.Fatalf("create error: naked=%v delay=%v held=%d", msg.naked, msg.nak, pool.held)
	}
}

func TestHandoff_CappedNaksAndReleases(t *testing.T) {
	pool := &fakePool{reserve: true}
	creator := &fakeCreator{outcome: Capped}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("build.failed", "obj/build", "core")))
	if !msg.naked || msg.nak != 60*time.Second || pool.held != 0 || len(pool.started) != 0 {
		t.Fatalf("capped: naked=%v held=%d started=%d", msg.naked, pool.held, len(pool.started))
	}
}

func TestHandoff_CreatedStartsAndAcks(t *testing.T) {
	pool := &fakePool{reserve: true}
	creator := &fakeCreator{outcome: Created}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("build.failed", "obj/build", "core")))
	if !msg.acked || msg.naked || len(pool.started) != 1 {
		t.Fatalf("created: acked=%v naked=%v started=%d", msg.acked, msg.naked, len(pool.started))
	}
}

func TestHandoff_DedupedAcksAndReleases(t *testing.T) {
	pool := &fakePool{reserve: true}
	creator := &fakeCreator{outcome: Deduped}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("build.failed", "obj/build", "core")))
	if !msg.acked || pool.held != 0 || len(pool.started) != 0 {
		t.Fatalf("deduped: acked=%v held=%d started=%d", msg.acked, pool.held, len(pool.started))
	}
}

func TestHandoff_UnmatchedAcks(t *testing.T) {
	pool := &fakePool{reserve: true}
	creator := &fakeCreator{outcome: Created}
	msg := &fakeMsg{}
	handoffFor(pool, creator).Handle(context.Background(), msg, mustJSON(t, rowOf("job.renamed", "obj/build", "core")))
	if !msg.acked || creator.calls != 0 {
		t.Fatalf("unmatched row: acked=%v calls=%d", msg.acked, creator.calls)
	}
}

func TestConsumerConfig(t *testing.T) {
	c := ConsumerConfig()
	if c.Durable != ConsumerName || c.FilterSubject != "activity.>" || c.MaxAckPending != 256 || c.MaxDeliver != -1 {
		t.Fatalf("consumer config = %+v", c)
	}
	if c.AckWait != 60*time.Second {
		t.Fatalf("AckWait = %v", c.AckWait)
	}
}
