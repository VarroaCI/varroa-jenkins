package sessions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	teamstore "github.com/varroaci/varroa-jenkins/internal/teamagent/store"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/testdb"
)

// seedWaitingSession creates one session that waits on an approval for one
// tool call, and no result yet.
func seedWaitingSession(t *testing.T, st *agentstore.Postgres) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	now := time.Now()
	call, err := agent.NewEvent(agent.EventToolCall, agent.ToolCallPayload{CallID: "c1", Name: "restart_run"})
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindTeamAgent, Principal: "agent:team-a/obj/triage", Team: "obj",
		Cluster: "core", Trigger: "run.failed", Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateWaitingApproval, StartedAt: now,
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{call}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return id
}

// addResult appends the tool result of call c1.
func addResult(t *testing.T, st *agentstore.Postgres, id uuid.UUID) {
	t.Helper()
	res, err := agent.NewEvent(agent.EventToolResult, agent.ToolResultPayload{CallID: "c1", Name: "restart_run", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(context.Background(), id, agentstore.Guard{}, res); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// resumerFor returns a resumer that counts the sessions it starts.
func resumerFor(st *agentstore.Postgres, pool *pgxpool.Pool, owner string, started *atomic.Int32) *Resumer {
	return &Resumer{
		Store:   st,
		Tenants: teamstore.New(pool, nil),
		Owner:   owner,
		Teams:   func() []string { return []string{"obj"} },
		Start: func(context.Context, agent.Session, loop.Trigger) {
			started.Add(1)
		},
		Window: 25 * time.Hour,
		Now:    time.Now,
	}
}

// TestResume_MissingToolResultBlocks proves that a transcript with an
// unanswered tool call does not resume, and that it resumes once the result is
// written.
func TestResume_MissingToolResultBlocks(t *testing.T) {
	pool, dsn := testdb.FreshDB(t)
	st, err := agentstore.Open(context.Background(), dsn, agentstore.Options{Now: time.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	id := seedWaitingSession(t, st)

	var started atomic.Int32
	r := resumerFor(st, pool, "pod-a", &started)
	if n, err := r.Scan(context.Background()); err != nil || n != 0 {
		t.Fatalf("scan before the result = %d, %v; want 0", n, err)
	}
	addResult(t, st, id)
	if n, err := r.Scan(context.Background()); err != nil || n != 1 {
		t.Fatalf("scan after the result = %d, %v; want 1", n, err)
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("starts = %d, want 1", got)
	}
}

// TestResume_TwoReplicasResumeOnce proves that two replicas that scan at once
// take one session between them.
func TestResume_TwoReplicasResumeOnce(t *testing.T) {
	pool, dsn := testdb.FreshDB(t)
	st, err := agentstore.Open(context.Background(), dsn, agentstore.Options{Now: time.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	id := seedWaitingSession(t, st)
	addResult(t, st, id)

	var a, b atomic.Int32
	ra := resumerFor(st, pool, "pod-a", &a)
	rb := resumerFor(st, pool, "pod-b", &b)
	var wg sync.WaitGroup
	for _, r := range []*Resumer{ra, rb} {
		wg.Add(1)
		go func(r *Resumer) {
			defer wg.Done()
			if _, err := r.Scan(context.Background()); err != nil {
				t.Errorf("Scan: %v", err)
			}
		}(r)
	}
	wg.Wait()
	if total := a.Load() + b.Load(); total != 1 {
		t.Fatalf("replicas resumed %d sessions, want 1", total)
	}
}
