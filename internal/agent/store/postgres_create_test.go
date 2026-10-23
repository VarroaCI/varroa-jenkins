package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// TestCreateUnderLock_OneWinsPerKey proves that two concurrent creates under
// one lock key run their check one after the other, so only one inserts.
func TestCreateUnderLock_OneWinsPerKey(t *testing.T) {
	p, err := Open(context.Background(), freshDSN(t), Options{Now: time.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, ok, err := p.CreateUnderLock(ctx, "tenantagent:ns/alpha:run.failed:job-a",
				func(ctx context.Context, tx pgx.Tx) (agent.Session, []agent.EventInput, bool, error) {
					// The check reads the table inside the lock. A second call
					// sees the row the first call wrote.
					var n int
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM varroa.agent_sessions WHERE tenant = 'alpha'`).Scan(&n); err != nil {
						return agent.Session{}, nil, false, err
					}
					if n > 0 {
						return agent.Session{}, nil, false, nil
					}
					s := newSession(agent.StateRunning, testOwner)
					s.Tenant = "alpha"
					s.Kind = agent.KindTenantAgent
					return s, []agent.EventInput{userEvent("go")}, true, nil
				})
			if err != nil {
				t.Errorf("CreateUnderLock: %v", err)
				return
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := created.Load(); got != 1 {
		t.Fatalf("created %d sessions under one key, want 1", got)
	}
}
