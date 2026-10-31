package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// factory builds a fresh store that reads its time from o.
type factory func(t *testing.T, o Options) Store

// runContract runs every contract subtest against the store that factory
// builds. The memory store and the Postgres store both run it.
func runContract(t *testing.T, f factory) {
	cases := []struct {
		name string
		fn   func(t *testing.T, f factory)
	}{
		{"CreateGet", testCreateGet},
		{"SkippedEndsAtStart", testSkippedEndsAtStart},
		{"ListSessionsCursorPaging", testListSessionsCursorPaging},
		{"ListSessionsStateFilter", testListSessionsStateFilter},
		{"DaySpendCurrentUTCDay", testDaySpendCurrentUTCDay},
		{"AppendEventConcurrentWriters", testAppendEventConcurrentWriters},
		{"EventsAfter", testEventsAfter},
		{"EndSessionStateGuard", testEndSessionStateGuard},
		{"TakeLeaseReentrantAndRefusesOther", testTakeLeaseReentrantAndRefusesOther},
		{"DeadReplicaLeaseLost", testDeadReplicaLeaseLost},
		{"LiveHolderUntouched", testLiveHolderUntouched},
		{"LostResumeEnds", testLostResumeEnds},
		{"ExpiryWins", testExpiryWins},
		{"ApprovalExpires", testApprovalExpires},
		{"DoubleResolutionOneWins", testDoubleResolutionOneWins},
		{"OnlyLastResolutionResumes", testOnlyLastResolutionResumes},
		{"NonOwnerNotFound", testNonOwnerNotFound},
		{"ExpiredUnsweptAnswersErrExpired", testExpiredUnsweptAnswersErrExpired},
		{"SweptExpiredAnswersErrExpired", testSweptExpiredAnswersErrExpired},
		{"GetApproval", testGetApproval},
		{"SettingsUserDefaults", testSettingsUserDefaults},
		{"SettingsPerFieldWrite", testSettingsPerFieldWrite},
		{"TwoAdminsTwoTenants", testTwoAdminsTwoTenants},
		{"SetInstructionsSHA", testSetInstructionsSHA},
		{"PutTenantAgentUnattended", testPutTenantAgentUnattended},
		{"SettingsCheckSeesMergedState", testSettingsCheckSeesMergedState},
		{"SettingsCheckRefusalWritesNothing", testSettingsCheckRefusalWritesNothing},
		{"SettingsCheckSerializesWriters", testSettingsCheckSerializesWriters},
		{"CancelDeniesPendingApprovals", testCancelDeniesPendingApprovals},
		{"CancelRunningWithApprovalsDenies", testCancelRunningWithApprovalsDenies},
		{"LeaseLostDeniesPendingApprovals", testLeaseLostDeniesPendingApprovals},
		{"ExpireDueSkipsEndedSessions", testExpireDueSkipsEndedSessions},
		{"ExpireDueDeniesRemainingApprovals", testExpireDueDeniesRemainingApprovals},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, f)
		})
	}
}

// userEvent returns a user event for seeding a transcript.
func userEvent(text string) agent.EventInput {
	ev, _ := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: text})
	return ev
}

// newSession builds a session row for the principal. A lease owner starts it
// as running under that lease.
func newSession(state agent.SessionState, owner string) agent.Session {
	return agent.Session{
		Kind:        agent.KindAssistant,
		Principal:   "alice",
		Cluster:     "core",
		Tenant:      "acme",
		Provider:    "smoke",
		Model:       "m",
		KeySource:   agent.KeySourceChart,
		DataSharing: agent.DataLogs,
		State:       state,
		LeaseOwner:  owner,
	}
}

// testOwner is the lease owner of every session that a contract subtest parks or resumes.
const testOwner = "pod-a"

// startRunning creates a running session under testOwner.
func startRunning(t *testing.T, s Store) agent.Session {
	t.Helper()
	sess, _, err := s.CreateSession(context.Background(), newSession(agent.StateRunning, testOwner),
		[]agent.EventInput{userEvent("hi")})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

// park gives a running session one approval request and parks it.
func park(t *testing.T, s Store, clock *testClock, sess agent.Session, expiresIn time.Duration) agent.Approval {
	t.Helper()
	ctx := context.Background()
	id, _ := agent.NewID()
	apps, _, err := s.CreateApprovals(ctx, sess.ID, testOwner, []agent.Approval{{
		ID: id, CallID: "call-1", Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(expiresIn),
	}}, nil)
	if err != nil {
		t.Fatalf("CreateApprovals: %v", err)
	}
	if _, err := s.ClearLease(ctx, sess.ID, testOwner, []agent.EventInput{userEvent("parked")}); err != nil {
		t.Fatalf("ClearLease: %v", err)
	}
	return apps[0]
}

func testCreateGet(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess, evs, err := s.CreateSession(ctx, newSession(agent.StateRunning, ""), []agent.EventInput{userEvent("hello")})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Fatalf("events = %+v, want one event at seq 1", evs)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Principal != "alice" || got.Trigger != agent.TriggerManual || got.State != agent.StateRunning {
		t.Fatalf("got %+v", got)
	}
	if !got.StartedAt.Equal(clock.Now()) {
		t.Fatalf("started_at = %s, want the test clock %s", got.StartedAt, clock.Now())
	}
	if _, err := s.GetSession(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session error = %v, want ErrNotFound", err)
	}
}

func testSkippedEndsAtStart(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := newSession(agent.StateSkipped, "")
	sess.SkipReason = agent.SkipBudgetDay
	got, _, err := s.CreateSession(ctx, sess, nil)
	if err != nil {
		t.Fatalf("CreateSession skipped: %v", err)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(got.StartedAt) {
		t.Fatalf("skipped ended_at = %v, want started_at %v", got.EndedAt, got.StartedAt)
	}
	bad := newSession(agent.StateSkipped, "")
	if _, _, err := s.CreateSession(ctx, bad, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("skipped without reason error = %v, want ErrInvalid", err)
	}
}

func testListSessionsCursorPaging(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	const n = 7
	want := map[uuid.UUID]bool{}
	for i := 0; i < n; i++ {
		sess, _, err := s.CreateSession(ctx, newSession(agent.StateCompleted, ""), nil)
		if err != nil {
			t.Fatalf("CreateSession %d: %v", i, err)
		}
		want[sess.ID] = true
		clock.Advance(time.Second)
	}
	var seen []uuid.UUID
	cursor := ""
	for {
		page, err := s.ListSessions(ctx, SessionQuery{Principal: "alice", Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		for _, it := range page.Items {
			seen = append(seen, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("paged %d sessions, want %d", len(seen), n)
	}
	unique := map[uuid.UUID]bool{}
	for _, id := range seen {
		if unique[id] || !want[id] {
			t.Fatalf("repeated or unknown session %s", id)
		}
		unique[id] = true
	}
}

func testListSessionsStateFilter(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	if _, _, err := s.CreateSession(ctx, newSession(agent.StateCompleted, ""), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateSession(ctx, newSession(agent.StateFailed, ""), nil); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSessions(ctx, SessionQuery{Principal: "alice", State: agent.StateFailed})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].State != agent.StateFailed {
		t.Fatalf("state filter returned %+v", page.Items)
	}
}

func testDaySpendCurrentUTCDay(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: func() time.Time { return clock.Now() }})
	ctx := context.Background()
	sess := newSession(agent.StateRunning, "pod-a")
	got, _, err := s.CreateSession(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordStep(ctx, got.ID, "pod-a", 100, 50); err != nil {
		t.Fatalf("RecordStep: %v", err)
	}
	total, err := s.DaySpend(ctx, "alice")
	if err != nil {
		t.Fatalf("DaySpend: %v", err)
	}
	if total != 150 {
		t.Fatalf("DaySpend = %d, want 150", total)
	}
	other, err := s.DaySpend(ctx, "bob")
	if err != nil || other != 0 {
		t.Fatalf("DaySpend bob = (%d, %v), want 0", other, err)
	}
}

func testAppendEventConcurrentWriters(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	const per = 50
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := s.AppendEvent(ctx, sess.ID, Guard{}, userEvent(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendEvent: %v", err)
	}
	evs, err := s.Events(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 1+2*per {
		t.Fatalf("events = %d, want %d", len(evs), 1+2*per)
	}
	for i, e := range evs {
		if e.Seq != i+1 {
			t.Fatalf("seq %d at position %d: gap or repeat", e.Seq, i)
		}
	}
}

func testEventsAfter(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	if _, err := s.AppendEvent(ctx, sess.ID, Guard{}, userEvent("two")); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(ctx, sess.ID, 1, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != 2 {
		t.Fatalf("events after 1 = %+v", evs)
	}
}

func testEndSessionStateGuard(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	if _, err := s.EndSession(ctx, sess.ID, EndInput{State: agent.StateRunning}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ending into running = %v, want ErrInvalid", err)
	}
	_, err := s.EndSession(ctx, sess.ID, EndInput{
		From: []agent.SessionState{agent.StateWaitingApproval}, State: agent.StateCompleted,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("end from the wrong state = %v, want ErrConflict", err)
	}
	if _, err := s.EndSession(ctx, sess.ID, EndInput{From: []agent.SessionState{agent.StateRunning}, State: agent.StateCompleted,
		Events: []agent.EventInput{mustDone(agent.StateCompleted)}}); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateCompleted || got.EndedAt == nil {
		t.Fatalf("after end = %+v", got)
	}
}

func mustDone(state agent.SessionState) agent.EventInput {
	ev, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: state})
	return ev
}

func testTakeLeaseReentrantAndRefusesOther(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess, _, err := s.CreateSession(ctx, newSession(agent.StateWaitingApproval, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	from := []agent.SessionState{agent.StateWaitingApproval}
	if _, err := s.TakeLease(ctx, sess.ID, "pod-a", from); err != nil {
		t.Fatalf("TakeLease pod-a: %v", err)
	}
	if _, err := s.TakeLease(ctx, sess.ID, "pod-a", []agent.SessionState{agent.StateRunning}); err != nil {
		t.Fatalf("TakeLease again by the holder: %v", err)
	}
	if _, err := s.TakeLease(ctx, sess.ID, "pod-b", []agent.SessionState{agent.StateRunning}); !errors.Is(err, ErrConflict) {
		t.Fatalf("TakeLease by another pod = %v, want ErrConflict", err)
	}
}

func testDeadReplicaLeaseLost(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	clock.Advance(LeaseTTL + time.Second)
	_, lost, err := s.Sweep(ctx)
	if err != nil || len(lost) != 1 {
		t.Fatalf("Sweep = lost %d, %v; want 1", len(lost), err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	evs, _ := s.Events(ctx, sess.ID, 0, 100)
	last2 := evs[len(evs)-2:]
	if last2[0].Type != agent.EventError || last2[1].Type != agent.EventDone {
		t.Fatalf("tail = %s, %s; want error then done", last2[0].Type, last2[1].Type)
	}
	if lost[0].ID != sess.ID || lost[0].Seq != evs[len(evs)-1].Seq {
		t.Fatalf("swept = %+v, want session %s at seq %d", lost[0], sess.ID, evs[len(evs)-1].Seq)
	}
	if !strings.Contains(string(last2[0].Payload), string(agent.CodeLeaseLost)) {
		t.Fatalf("error payload %s lacks lease_lost", last2[0].Payload)
	}
}

func testLiveHolderUntouched(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	clock.Advance(LeaseTTL - time.Second)
	if err := s.RenewLease(ctx, sess.ID, "pod-a"); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	clock.Advance(LeaseTTL - time.Second)
	if _, lost, err := s.Sweep(ctx); err != nil || len(lost) != 0 {
		t.Fatalf("Sweep of a live holder = lost %d, %v", len(lost), err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateRunning {
		t.Fatalf("live holder state = %s", got.State)
	}
}

func testLostResumeEnds(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, 30*time.Minute)
	if _, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "alice", Owner: "alice"}); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	clock.Advance(6 * time.Minute)
	if _, lost, err := s.Sweep(ctx); err != nil || len(lost) != 1 {
		t.Fatalf("Sweep = lost %d, %v; want 1", len(lost), err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	evs, _ := s.Events(ctx, sess.ID, 0, 100)
	if !strings.Contains(string(evs[len(evs)-2].Payload), "may have run") {
		t.Fatalf("error text lacks the resume warning: %s", evs[len(evs)-2].Payload)
	}
	ap, _ := s.GetApproval(ctx, app.ID)
	if ap.State != agent.ApprovalApproved {
		t.Fatalf("approval state = %s, want approved", ap.State)
	}
}

func testExpiryWins(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, time.Minute)
	clock.Advance(2 * time.Minute)
	_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "alice", Owner: "alice"})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("resolution after expiry = %v, want ErrExpired", err)
	}
}

func testApprovalExpires(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, time.Minute)
	clock.Advance(2 * time.Minute)
	ended, err := s.ExpireDue(ctx)
	if err != nil || len(ended) != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1", len(ended), err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateExpired {
		t.Fatalf("session state = %s, want expired", got.State)
	}
	evs, _ := s.Events(ctx, sess.ID, 0, 100)
	if ended[0].ID != sess.ID || ended[0].Seq != evs[len(evs)-1].Seq {
		t.Fatalf("ended = %+v, want session %s at seq %d", ended[0], sess.ID, evs[len(evs)-1].Seq)
	}
	ap, _ := s.GetApproval(ctx, app.ID)
	if ap.State != agent.ApprovalExpired {
		t.Fatalf("approval state = %s, want expired", ap.State)
	}
	if again, err := s.ExpireDue(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second ExpireDue = %d, %v; want 0", len(again), err)
	}
}

func testDoubleResolutionOneWins(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, 10*time.Minute)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "alice", Owner: "alice"})
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("resolutions that won = %d, want 1", wins)
	}
}

func testOnlyLastResolutionResumes(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	a1 := park(t, s, clock, sess, 10*time.Minute)

	// A parked session takes no approvals until a holder resumes it.
	id2, _ := agent.NewID()
	if _, _, err := s.CreateApprovals(ctx, sess.ID, "pod-b", []agent.Approval{{
		ID: id2, CallID: "call-2", Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(10 * time.Minute),
	}}, nil); err == nil {
		t.Fatal("CreateApprovals on a parked session succeeded")
	}
	if _, err := s.TakeLease(ctx, sess.ID, "pod-b", []agent.SessionState{agent.StateWaitingApproval}); err != nil {
		t.Fatalf("TakeLease: %v", err)
	}
	a2 := park2(t, s, clock, sess, "pod-b")

	r1, err := s.ResolveApproval(ctx, a1.ID, Resolution{State: agent.ApprovalApproved, By: "alice", Owner: "alice"})
	if err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	if r1.PendingLeft != 1 {
		t.Fatalf("PendingLeft after first = %d, want 1", r1.PendingLeft)
	}
	r2, err := s.ResolveApproval(ctx, a2.ID, Resolution{State: agent.ApprovalDenied, By: "alice", Owner: "alice"})
	if err != nil {
		t.Fatalf("second resolution: %v", err)
	}
	if r2.PendingLeft != 0 {
		t.Fatalf("PendingLeft after last = %d, want 0", r2.PendingLeft)
	}
}

// park2 gives a resumed session one more approval and parks it again.
func park2(t *testing.T, s Store, clock *testClock, sess agent.Session, owner string) agent.Approval {
	t.Helper()
	ctx := context.Background()
	id, _ := agent.NewID()
	apps, _, err := s.CreateApprovals(ctx, sess.ID, owner, []agent.Approval{{
		ID: id, CallID: "call-2", Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(10 * time.Minute),
	}}, nil)
	if err != nil {
		t.Fatalf("CreateApprovals on resume: %v", err)
	}
	if _, err := s.ClearLease(ctx, sess.ID, owner, nil); err != nil {
		t.Fatalf("ClearLease on resume: %v", err)
	}
	return apps[0]
}

func testNonOwnerNotFound(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, 10*time.Minute)
	_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "mallory", Owner: "mallory"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner resolution = %v, want ErrNotFound", err)
	}
}

func testExpiredUnsweptAnswersErrExpired(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, time.Minute)
	clock.Advance(2 * time.Minute)
	_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalDenied, By: "alice", Owner: "alice"})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("resolution of an unswept expired approval = %v, want ErrExpired", err)
	}
}

func testSweptExpiredAnswersErrExpired(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, time.Minute)
	clock.Advance(2 * time.Minute)
	if swept, err := s.ExpireDue(ctx); err != nil || len(swept) != 1 {
		t.Fatalf("ExpireDue = %v, %v; want one swept session", swept, err)
	}
	for _, state := range []agent.ApprovalState{agent.ApprovalApproved, agent.ApprovalDenied} {
		_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: state, By: "alice", Owner: "alice"})
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("resolution %s of a swept approval = %v, want ErrExpired", state, err)
		}
	}
	_, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "mallory", Owner: "mallory"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner resolution of a swept approval = %v, want ErrNotFound", err)
	}
}

func testGetApproval(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, 10*time.Minute)
	got, err := s.GetApproval(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.SessionID != sess.ID || got.Tool != "run_pipeline" || got.State != agent.ApprovalPending {
		t.Fatalf("approval = %+v", got)
	}
	if _, err := s.GetApproval(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing approval = %v, want ErrNotFound", err)
	}
}

func testSettingsUserDefaults(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	st, err := s.GetSettings(ctx, agent.UserScope("alice"))
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Stored || !st.Enabled || st.DataSharing != agent.DataLogs {
		t.Fatalf("user defaults = %+v", st)
	}
	if _, err := s.GetSettings(ctx, "bogus"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad scope = %v, want ErrInvalid", err)
	}
}

func testSettingsPerFieldWrite(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	paused := true
	st, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{Paused: &paused}, "ops")
	if err != nil {
		t.Fatalf("PutSettings paused: %v", err)
	}
	if !st.Paused || st.PausedBy != "ops" || st.PausedAt == nil {
		t.Fatalf("paused settings = %+v", st)
	}
	model := "m2"
	st, err = s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{DefaultModel: &model}, "ops")
	if err != nil {
		t.Fatalf("PutSettings model: %v", err)
	}
	if !st.Paused || st.DefaultModel != "m2" {
		t.Fatalf("a second field write changed the first: %+v", st)
	}
	if _, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{Enabled: &paused}, "ops"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user field on global = %v, want ErrInvalid", err)
	}
}

func testTwoAdminsTwoTenants(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	payments := "payments"
	billing := "billing"
	if _, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{
		TenantSet: &agent.TenantLevel{Tenant: billing, Level: agent.DataLogs},
	}, "admin-a"); err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{
				TenantSet: &agent.TenantLevel{Tenant: payments, Level: agent.DataMetadata},
			}, "admin-a")
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{TenantRemove: &billing}, "admin-b")
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
	st, err := s.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.TenantDataSharing[payments] != agent.DataMetadata {
		t.Fatalf("payments level = %q, want metadata", st.TenantDataSharing[payments])
	}
	if _, ok := st.TenantDataSharing[billing]; ok {
		t.Fatal("billing was not removed")
	}
}

// errRefused is the sentinel a settings check returns in these tests.
var errRefused = errors.New("test: refused")

func testSettingsCheckSeesMergedState(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	alpha, a1 := "alpha", "a1"
	if _, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{DefaultProvider: &alpha, DefaultModel: &a1}, "ops"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b1 := "b1"
	var seen agent.Settings
	_, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{
		DefaultModel: &b1,
		Check: func(after agent.Settings) error {
			seen = after
			return nil
		},
	}, "ops")
	if err != nil {
		t.Fatalf("put model: %v", err)
	}
	if seen.DefaultProvider != "alpha" || seen.DefaultModel != "b1" {
		t.Fatalf("check saw %s/%s, want alpha/b1", seen.DefaultProvider, seen.DefaultModel)
	}
	beta := "beta"
	_, err = s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{
		DefaultProvider: &beta,
		Check:           func(agent.Settings) error { return errRefused },
	}, "ops")
	if !errors.Is(err, errRefused) {
		t.Fatalf("refused put = %v, want errRefused", err)
	}
	st, err := s.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.DefaultProvider != "alpha" || st.DefaultModel != "b1" {
		t.Fatalf("refused write changed the scope: %s/%s", st.DefaultProvider, st.DefaultModel)
	}
}

func testSettingsCheckRefusalWritesNothing(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	x := "x"
	_, err := s.PutSettings(ctx, agent.UserScope("carol"), SettingsPatch{
		Provider: &x,
		Check:    func(agent.Settings) error { return errRefused },
	}, "carol")
	if !errors.Is(err, errRefused) {
		t.Fatalf("refused put = %v, want errRefused", err)
	}
	st, err := s.GetSettings(ctx, agent.UserScope("carol"))
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Stored || st.Provider != "" {
		t.Fatalf("refused first write left a row: %+v", st)
	}
}

// testSettingsCheckSerializesWriters reproduces the race where a check reads
// a snapshot that another writer changes before this writer commits.
func testSettingsCheckSerializesWriters(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	alpha, a1 := "alpha", "a1"
	if _, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{DefaultProvider: &alpha, DefaultModel: &a1}, "ops"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	empty := ""
	doneB := make(chan error, 1)
	go func() {
		_, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{DefaultProvider: &empty, DefaultModel: &empty,
			Check: func(agent.Settings) error {
				close(entered)
				<-release
				return nil
			}}, "ops")
		doneB <- err
	}()
	<-entered

	onlyModel := "a1"
	doneA := make(chan error, 1)
	go func() {
		_, err := s.PutSettings(ctx, agent.ScopeGlobal, SettingsPatch{DefaultModel: &onlyModel,
			Check: func(a agent.Settings) error {
				if a.DefaultModel != "" && a.DefaultProvider == "" {
					return errRefused
				}
				return nil
			}}, "ops")
		doneA <- err
	}()
	select {
	case err := <-doneA:
		t.Fatalf("second writer finished before the first committed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	if err := <-doneB; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := <-doneA; !errors.Is(err, errRefused) {
		t.Fatalf("second writer = %v, want errRefused", err)
	}
	st, err := s.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.DefaultProvider != "" || st.DefaultModel != "" {
		t.Fatalf("stored pair = %q/%q, want empty", st.DefaultProvider, st.DefaultModel)
	}
}

// lastEvents returns the last n events of a session.
func lastEvents(t *testing.T, s Store, id uuid.UUID, n int) []agent.Event {
	t.Helper()
	evs, err := s.Events(context.Background(), id, 0, 1000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) < n {
		t.Fatalf("got %d events, want at least %d", len(evs), n)
	}
	return evs[len(evs)-n:]
}

func testCancelDeniesPendingApprovals(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	app := park(t, s, clock, sess, 30*time.Minute)
	if _, err := s.EndSession(ctx, sess.ID, EndInput{
		From:   []agent.SessionState{agent.StateRunning, agent.StateWaitingApproval},
		By:     "alice",
		State:  agent.StateCancelled,
		Events: []agent.EventInput{mustDone(agent.StateCancelled)},
	}); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	got, err := s.GetApproval(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.State != agent.ApprovalDenied || got.ResolvedBy != "alice" || got.ResolvedAt == nil {
		t.Fatalf("approval = state %s by %q resolved %v; want denied by alice", got.State, got.ResolvedBy, got.ResolvedAt)
	}
	if got.Reason != "the session ended as cancelled" {
		t.Fatalf("reason = %q", got.Reason)
	}
	page, err := s.ListApprovals(ctx, ApprovalQuery{Principal: "alice", State: agent.ApprovalPending})
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("pending approvals after cancel = %d, want 0", len(page.Items))
	}
	evs := lastEvents(t, s, sess.ID, 2)
	if evs[0].Type != agent.EventApprovalResolved || evs[1].Type != agent.EventDone {
		t.Fatalf("tail = %s, %s; want approval_resolved then done", evs[0].Type, evs[1].Type)
	}
	var resolved agent.ApprovalResolvedPayload
	if err := json.Unmarshal(evs[0].Payload, &resolved); err != nil {
		t.Fatalf("decode approval_resolved: %v", err)
	}
	if resolved.State != agent.ApprovalDenied || resolved.ApprovalID != app.ID.String() {
		t.Fatalf("approval_resolved = %+v, want denied for %s", resolved, app.ID)
	}
	if _, err := s.ResolveApproval(ctx, app.ID, Resolution{State: agent.ApprovalApproved, By: "alice", Owner: "alice"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("ResolveApproval after cancel = %v, want ErrConflict", err)
	}
}

func testCancelRunningWithApprovalsDenies(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	id, _ := agent.NewID()
	apps, _, err := s.CreateApprovals(ctx, sess.ID, testOwner, []agent.Approval{{
		ID: id, CallID: "call-1", Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(30 * time.Minute),
	}}, nil)
	if err != nil {
		t.Fatalf("CreateApprovals: %v", err)
	}
	if _, err := s.EndSession(ctx, sess.ID, EndInput{
		From:   []agent.SessionState{agent.StateRunning},
		State:  agent.StateCancelled,
		Events: []agent.EventInput{mustDone(agent.StateCancelled)},
	}); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	got, err := s.GetApproval(ctx, apps[0].ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.State != agent.ApprovalDenied || got.ResolvedBy != "" {
		t.Fatalf("approval = state %s by %q; want denied with no actor", got.State, got.ResolvedBy)
	}
}

func testLeaseLostDeniesPendingApprovals(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	id, _ := agent.NewID()
	apps, _, err := s.CreateApprovals(ctx, sess.ID, testOwner, []agent.Approval{{
		ID: id, CallID: "call-1", Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(30 * time.Minute),
	}}, nil)
	if err != nil {
		t.Fatalf("CreateApprovals: %v", err)
	}
	clock.Advance(LeaseTTL + time.Second)
	if _, lost, err := s.Sweep(ctx); err != nil || len(lost) != 1 {
		t.Fatalf("Sweep = lost %d, %v; want 1", len(lost), err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	ap, err := s.GetApproval(ctx, apps[0].ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if ap.State != agent.ApprovalDenied || ap.ResolvedBy != "" {
		t.Fatalf("approval = state %s by %q; want denied with no actor", ap.State, ap.ResolvedBy)
	}
	evs := lastEvents(t, s, sess.ID, 3)
	if evs[0].Type != agent.EventApprovalResolved || evs[1].Type != agent.EventError || evs[2].Type != agent.EventDone {
		t.Fatalf("tail = %s, %s, %s; want approval_resolved, error, done", evs[0].Type, evs[1].Type, evs[2].Type)
	}
}

// seedOrphan creates a session that is cancelled while its approval stays
// pending. The approval expires one minute after the clock reads now.
func seedOrphan(t *testing.T, s Store, clock *testClock, i int) {
	t.Helper()
	// The direct SQL below needs the concrete store, so unwrap a deadline wrapper.
	if w, ok := s.(*timeoutStore); ok {
		s = w.inner
	}
	ctx := context.Background()
	sess := startRunning(t, s)
	id, _ := agent.NewID()
	if _, _, err := s.CreateApprovals(ctx, sess.ID, testOwner, []agent.Approval{{
		ID: id, CallID: fmt.Sprintf("orphan-%d", i), Tool: "run_pipeline", Arguments: []byte(`{}`),
		TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(time.Minute),
	}}, nil); err != nil {
		t.Fatalf("CreateApprovals orphan %d: %v", i, err)
	}
	now := clock.Now()
	switch st := s.(type) {
	case *Memory:
		st.mu.Lock()
		st.sessions[sess.ID].State = agent.StateCancelled
		st.sessions[sess.ID].EndedAt = &now
		st.mu.Unlock()
	case *Postgres:
		if _, err := st.pool.Exec(ctx, `UPDATE varroa.agent_sessions SET state = 'cancelled', ended_at = $2 WHERE id = $1`, sess.ID, now); err != nil {
			t.Fatalf("cancel orphan session: %v", err)
		}
	default:
		t.Fatalf("seedOrphan: unknown store %T", s)
	}
}

func testExpireDueSkipsEndedSessions(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	for i := range 101 {
		seedOrphan(t, s, clock, i)
	}
	sess := startRunning(t, s)
	park(t, s, clock, sess, time.Minute)
	clock.Advance(2 * time.Minute)
	ended, err := s.ExpireDue(ctx)
	if err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if len(ended) != 1 || ended[0].ID != sess.ID {
		t.Fatalf("ExpireDue ended %+v, want only session %s", ended, sess.ID)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.State != agent.StateExpired {
		t.Fatalf("state = %s, want expired", got.State)
	}
}

func testExpireDueDeniesRemainingApprovals(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)
	dueID, _ := agent.NewID()
	laterID, _ := agent.NewID()
	if _, _, err := s.CreateApprovals(ctx, sess.ID, testOwner, []agent.Approval{
		{ID: dueID, CallID: "call-1", Tool: "run_pipeline", Arguments: []byte(`{}`), TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(time.Minute)},
		{ID: laterID, CallID: "call-2", Tool: "run_pipeline", Arguments: []byte(`{}`), TargetKind: agent.TargetNone, ExpiresAt: clock.Now().Add(time.Hour)},
	}, nil); err != nil {
		t.Fatalf("CreateApprovals: %v", err)
	}
	if _, err := s.ClearLease(ctx, sess.ID, testOwner, nil); err != nil {
		t.Fatalf("ClearLease: %v", err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := s.ExpireDue(ctx); err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if ap, _ := s.GetApproval(ctx, dueID); ap.State != agent.ApprovalExpired {
		t.Fatalf("due approval = %s, want expired", ap.State)
	}
	ap, _ := s.GetApproval(ctx, laterID)
	if ap.State != agent.ApprovalDenied || ap.Reason != "the session ended as expired" {
		t.Fatalf("remaining approval = state %s reason %q; want denied as expired", ap.State, ap.Reason)
	}
}
