package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	teamstore "github.com/varroaci/varroa-jenkins/internal/teamagent/store"
)

// resumePage bounds one events read of the resume scan.
const resumePage = 500

// Resumer finds the Team agent sessions that a replica must take over, and
// starts them. A session waits on an approval, or its lease ran out, and it
// may resume only when every tool call of its transcript has a result.
type Resumer struct {
	Store   agentstore.Store
	Tenants *teamstore.Store
	Owner   string
	// Teams returns the names of the Teams that have an accepted TeamAgent.
	Teams func() []string
	// Reserve takes a worker slot before a lease is taken. A full pool skips the
	// session, which stays waiting for the next scan. Release frees the slot of a
	// take that did not start. Start takes over the slot of a taken session.
	Reserve func() bool
	Release func()
	// Start runs a taken-over session with the resume trigger.
	Start func(ctx context.Context, sess agent.Session, trigger loop.Trigger)
	// Window bounds how far back the scan looks: the approval expiry plus an hour.
	Window time.Duration
	Now    func() time.Time
}

// Scan runs one resume pass and returns the number of sessions it started.
func (r *Resumer) Scan(ctx context.Context) (int, error) {
	now := r.Now()
	teams := r.Teams()
	if len(teams) == 0 {
		return 0, nil
	}
	ids, err := r.Tenants.ResumeCandidates(ctx, teams, now.Add(-r.Window))
	if err != nil {
		return 0, err
	}
	started := 0
	for _, id := range ids {
		ok, err := r.resumeOne(ctx, id)
		if err != nil {
			return started, err
		}
		if ok {
			started++
		}
	}
	return started, nil
}

// resumeOne takes over one session when its transcript is complete. The worker
// slot is reserved first, so a lease is never held by a replica with no worker.
func (r *Resumer) resumeOne(ctx context.Context, id uuid.UUID) (bool, error) {
	if r.Reserve != nil && !r.Reserve() {
		return false, nil
	}
	release := func() {
		if r.Release != nil {
			r.Release()
		}
	}
	complete, err := transcriptComplete(ctx, r.Store, id)
	if err != nil || !complete {
		release()
		return false, err
	}
	sess, err := r.Store.TakeLease(ctx, id, r.Owner, []agent.SessionState{agent.StateWaitingApproval, agent.StateRunning})
	if errors.Is(err, agentstore.ErrConflict) {
		release()
		return false, nil
	}
	if err != nil {
		release()
		return false, fmt.Errorf("take lease: %w", err)
	}
	r.Start(ctx, sess, loop.TriggerResume)
	return true, nil
}

// transcriptComplete reports whether every tool call of the session has a
// tool result. A missing result means an approval was resolved but its result
// was not yet written, so resuming now would replay the call without its answer.
func transcriptComplete(ctx context.Context, st agentstore.Store, id uuid.UUID) (bool, error) {
	calls := map[string]bool{}
	results := map[string]bool{}
	after := 0
	for {
		evs, err := st.Events(ctx, id, after, resumePage)
		if err != nil {
			return false, fmt.Errorf("read events: %w", err)
		}
		if len(evs) == 0 {
			break
		}
		for _, e := range evs {
			after = e.Seq
			switch e.Type {
			case agent.EventToolCall:
				var p agent.ToolCallPayload
				if json.Unmarshal(e.Payload, &p) == nil {
					calls[p.CallID] = true
				}
			case agent.EventToolResult:
				var p agent.ToolResultPayload
				if json.Unmarshal(e.Payload, &p) == nil {
					results[p.CallID] = true
				}
			}
		}
		if len(evs) < resumePage {
			break
		}
	}
	for c := range calls {
		if !results[c] {
			return false, nil
		}
	}
	return true, nil
}
