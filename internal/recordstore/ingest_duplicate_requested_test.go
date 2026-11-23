package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// backstopTestRig is one database, one ingester, and the ledger actions
// that the ingester publishes on the test cluster.
type backstopTestRig struct {
	pool    *pgxpool.Pool
	g       *Ingester
	actions <-chan LedgerAction
}

// newBackstopTestRig opens a test database and an ingester that publishes
// on an embedded NATS server.
func newBackstopTestRig(t *testing.T) *backstopTestRig {
	t.Helper()
	pool, _ := setupLedgerPool(t)
	s := startDuplicateTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	actions := subscribeLedgerActionsForTest(t, conn)
	return &backstopTestRig{pool: pool, g: NewIngester(pool, conn, nil), actions: actions}
}

// start applies run.started for runID with the build number its grant
// holds. A non-empty revision goes on the payload as scm_revision. params
// goes on the payload when it is not nil.
func (r *backstopTestRig) start(t *testing.T, runID uuid.UUID, key, revision string, params map[string]string) {
	t.Helper()
	var number int64
	if err := r.pool.QueryRow(context.Background(),
		`SELECT build_number FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&number); err != nil {
		t.Fatalf("read granted number of %s: %v", key, err)
	}
	payload := map[string]any{"build_number": number}
	if revision != "" {
		payload["scm_revision"] = revision
	}
	if params != nil {
		payload["params"] = params
	}
	if _, err := r.g.Apply(context.Background(), newTestEnvelope(t, runID, key, KindRunStarted, payload)); err != nil {
		t.Fatalf("apply run.started %s: %v", key, err)
	}
}

// source applies run.source for runID, which writes revision onto the
// stored row the way the plugin's first-node probe does.
func (r *backstopTestRig) source(t *testing.T, runID uuid.UUID, key, revision string) {
	t.Helper()
	env := newTestEnvelope(t, runID, key, KindRunSource, RunSourcePayload{Seq: 1, SCMRevision: &revision})
	if _, err := r.g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.source %s: %v", key, err)
	}
}

// wantStatus fails the test when runID's stored status is not want.
func (r *backstopTestRig) wantStatus(t *testing.T, name string, runID uuid.UUID, want string) {
	t.Helper()
	if got := runStatus(t, r.g, runID); got != want {
		t.Fatalf("%s: status of %s = %q, want %q", name, runID, got, want)
	}
}

// wantNoAction fails the test when the ingester published a ledger action.
func (r *backstopTestRig) wantNoAction(t *testing.T, name string) {
	t.Helper()
	select {
	case a := <-r.actions:
		t.Fatalf("%s: published %s for run %s, want no action", name, a.Kind, a.RunID)
	case <-time.After(200 * time.Millisecond):
	}
}

// wantCancel fails the test unless the next ledger action is one
// cancel_run for runID.
func (r *backstopTestRig) wantCancel(t *testing.T, name string, runID uuid.UUID) {
	t.Helper()
	select {
	case a := <-r.actions:
		if a.Kind != LedgerActionCancelRun || a.RunID != runID.String() {
			t.Fatalf("%s: published %s for run %s, want cancel_run for %s", name, a.Kind, a.RunID, runID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no cancel_run action was published", name)
	}
}

// TestDuplicateBackstopKeepsVarroaBuildRunBesideSCMRun asserts that a
// varroa.build run of a branch job keeps running when an older push build
// of the same commit starts after it. The push build was admitted first
// and waited for its revision. The varroa.build run started at once, and
// its run.source wrote the commit onto its row before the push build's
// run.started arrived.
func TestDuplicateBackstopKeepsVarroaBuildRunBesideSCMRun(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-varroa-build"
	pushKey := "scm:acme/app:" + revision
	buildKey := "upstream:" + newTestUUIDv7String() + ":s1"

	push := seedGrantedIntent(t, r.pool, pushKey)
	build := seedGrantedIntent(t, r.pool, buildKey)

	r.start(t, build, buildKey, "", map[string]string{"TARGET": "prod"})
	r.source(t, build, buildKey, revision)
	r.start(t, push, pushKey, revision, nil)

	r.wantStatus(t, "varroa.build run", build, StatusRunning)
	r.wantStatus(t, "push run", push, StatusRunning)
	r.wantNoAction(t, "varroa.build beside a push build")
}

// TestDuplicateBackstopKeepsTwoVarroaBuildRunsOfOneRevision asserts that
// two varroa.build calls on one job keep both runs when both runs report
// one revision. The two callers pass other parameters, and each caller
// waits on its own run.
func TestDuplicateBackstopKeepsTwoVarroaBuildRunsOfOneRevision(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-two-producers"
	keyA := "upstream:" + newTestUUIDv7String() + ":s1"
	keyB := "upstream:" + newTestUUIDv7String() + ":s1"

	runA := seedGrantedIntent(t, r.pool, keyA)
	runB := seedGrantedIntent(t, r.pool, keyB)

	r.start(t, runA, keyA, revision, map[string]string{"TARGET": "staging"})
	r.start(t, runB, keyB, revision, map[string]string{"TARGET": "prod"})

	r.wantStatus(t, "first varroa.build run", runA, StatusRunning)
	r.wantStatus(t, "second varroa.build run", runB, StatusRunning)
	r.wantNoAction(t, "two varroa.build runs")
}

// TestDuplicateBackstopLeavesRequestedRunsAlone asserts that a run whose
// intent key Varroa supplies is never marked and never makes another run
// a duplicate. Each case pairs one requested run with one push build of
// the same revision, in three orders.
func TestDuplicateBackstopLeavesRequestedRunsAlone(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	requested := []struct {
		scheme string
		key    func() string
	}{
		{"manual", func() string { return "manual:" + newTestUUIDv7String() }},
		{"retry", func() string { return "retry:" + newTestUUIDv7String() }},
		{"upstream", func() string { return "upstream:" + newTestUUIDv7String() + ":1" }},
		{"replay", func() string { return "replay:" + newTestUUIDv7String() + ":" + newTestUUIDv7String() }},
		{"restart", func() string { return "restart:" + newTestUUIDv7String() + ":" + newTestUUIDv7String() }},
	}

	for _, rq := range requested {
		// The requested run is older and live. Its run.source wrote the
		// revision. A newer push build then starts.
		name := rq.scheme + " older, push newer"
		revision := "rev-" + rq.scheme + "-older"
		reqKey, pushKey := rq.key(), "scm:acme/app:"+revision
		reqRun := seedGrantedIntent(t, r.pool, reqKey)
		pushRun := seedGrantedIntent(t, r.pool, pushKey)
		r.start(t, reqRun, reqKey, "", nil)
		r.source(t, reqRun, reqKey, revision)
		r.start(t, pushRun, pushKey, revision, nil)
		r.wantStatus(t, name, reqRun, StatusRunning)
		r.wantStatus(t, name, pushRun, StatusRunning)
		r.wantNoAction(t, name)

		// The push build is older, but the requested run started first.
		name = rq.scheme + " newer, push starts last"
		revision = "rev-" + rq.scheme + "-newer"
		reqKey, pushKey = rq.key(), "scm:acme/app:"+revision
		pushRun = seedGrantedIntent(t, r.pool, pushKey)
		reqRun = seedGrantedIntent(t, r.pool, reqKey)
		r.start(t, reqRun, reqKey, "", nil)
		r.source(t, reqRun, reqKey, revision)
		r.start(t, pushRun, pushKey, revision, nil)
		r.wantStatus(t, name, reqRun, StatusRunning)
		r.wantStatus(t, name, pushRun, StatusRunning)
		r.wantNoAction(t, name)

		// The requested run's own run.started reports the revision of a
		// live push build.
		name = rq.scheme + " arrives with a revision"
		revision = "rev-" + rq.scheme + "-arriving"
		reqKey, pushKey = rq.key(), "scm:acme/app:"+revision
		pushRun = seedGrantedIntent(t, r.pool, pushKey)
		reqRun = seedGrantedIntent(t, r.pool, reqKey)
		r.start(t, pushRun, pushKey, revision, nil)
		r.start(t, reqRun, reqKey, revision, nil)
		r.wantStatus(t, name, reqRun, StatusRunning)
		r.wantStatus(t, name, pushRun, StatusRunning)
		r.wantNoAction(t, name)
	}
}

// TestDuplicateBackstopMarksJenkinsScheduledRuns asserts that the backstop
// still stops a second run that Jenkins scheduled for one commit. A branch
// event whose revision wait ran out builds under an api: key, and a branch
// scan or a timer can build the same commit beside it.
func TestDuplicateBackstopMarksJenkinsScheduledRuns(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	cases := []struct {
		name     string
		olderKey string
		// sourceFirst seeds the scm: run first, but applies the other
		// run's run.started and run.source before the scm: run.started.
		sourceFirst bool
	}{
		{name: "api older, scm newer", olderKey: "api:" + newTestUUIDv7String()},
		{name: "cron older, scm newer", olderKey: "cron:acme/app:2026-11-23T10:00:00Z"},
		{name: "scm older, api newer starts first", olderKey: "api:" + newTestUUIDv7String(), sourceFirst: true},
	}

	for i, tc := range cases {
		revision := "rev-scheduled-" + string(rune('a'+i))
		scmKey := "scm:acme/app:" + revision
		otherKey := tc.olderKey

		var scmRun, otherRun, newer uuid.UUID
		if tc.sourceFirst {
			scmRun = seedGrantedIntent(t, r.pool, scmKey)
			otherRun = seedGrantedIntent(t, r.pool, otherKey)
			newer = otherRun
		} else {
			otherRun = seedGrantedIntent(t, r.pool, otherKey)
			scmRun = seedGrantedIntent(t, r.pool, scmKey)
			newer = scmRun
		}
		r.start(t, otherRun, otherKey, "", nil)
		r.source(t, otherRun, otherKey, revision)
		r.start(t, scmRun, scmKey, revision, nil)

		older := scmRun
		if newer == scmRun {
			older = otherRun
		}
		r.wantStatus(t, tc.name, newer, StatusDuplicate)
		r.wantStatus(t, tc.name, older, StatusRunning)
		r.wantCancel(t, tc.name, newer)
	}
}
