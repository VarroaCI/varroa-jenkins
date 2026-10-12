package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// capWaiterLive is how long a wait row stays live after its last claim
// refresh. A capped run retries every 10 seconds, so three missed
// retries end the wait.
const capWaiterLive = 30 * time.Second

// capGrantWindow is how long a granted intent holds a slot. It matches
// the claim-expiry age, so a grant that no run claimed in time holds
// nothing.
const capGrantWindow = ClaimExpiryAge

// capRunningWindow bounds the RUNNING runs that hold a slot. A run row
// older than this is not counted, so a row that never ended does not
// hold a slot forever.
const capRunningWindow = 30 * 24 * time.Hour

// capKindTenant and capKindCell name the two cap kinds of a wait row.
const (
	capKindTenant = "tenant"
	capKindCell   = "cell"
)

// capVerdict is the answer of the cap check for one claim.
type capVerdict struct {
	Blocked bool
	Kind    string
	Name    string
	Max     int
	Running int
	Message string
}

// inFlightRun is one build that holds a slot of a tenant's cap.
type inFlightRun struct {
	runID   uuid.UUID
	jobPath string
	cell    string
}

// capWaiter is one live wait row of a tenant. It names the cap that last
// blocked the run and the time the run first waited.
type capWaiter struct {
	runID     uuid.UUID
	jobPath   string
	cell      string
	kind      string
	name      string
	firstSeen time.Time
}

// olderWaiter reports whether waiter w queues ahead of the run that holds
// runID and first waited at mine. Ties on the first-seen time break on
// the run id, so two waiters always order one way.
func olderWaiter(w capWaiter, runID uuid.UUID, mine time.Time) bool {
	if !w.firstSeen.Equal(mine) {
		return w.firstSeen.Before(mine)
	}
	return w.runID.String() < runID.String()
}

// decideCaps answers whether one claim waits behind a concurrency cap. It
// checks each matching tenant limit in spec order, then the cell cap. A
// live older waiter on the same limit counts against that limit, so a
// freed slot goes to the run that waited longest.
func decideCaps(tenant string, req ClaimRequest, runID uuid.UUID, mine time.Time,
	limits []ConcurrencyLimit, cellMax int, inflight []inFlightRun, waiters []capWaiter) capVerdict {
	for _, l := range limits {
		running := 0
		for _, r := range inflight {
			if limitMatches(tenant, r.jobPath, l) {
				running++
			}
		}
		ahead := 0
		for _, w := range waiters {
			if w.kind == capKindTenant && w.name == l.Name && olderWaiter(w, runID, mine) {
				ahead++
			}
		}
		if running+ahead >= l.Max {
			return capVerdict{Blocked: true, Kind: capKindTenant, Name: l.Name, Max: l.Max, Running: running,
				Message: fmt.Sprintf("waiting on concurrency limit %q: %d of %d builds run", l.Name, running, l.Max)}
		}
	}
	if cellMax > 0 {
		running := 0
		for _, r := range inflight {
			if r.cell == req.Cell {
				running++
			}
		}
		ahead := 0
		for _, w := range waiters {
			if w.kind == capKindCell && w.cell == req.Cell && olderWaiter(w, runID, mine) {
				ahead++
			}
		}
		if running+ahead >= cellMax {
			return capVerdict{Blocked: true, Kind: capKindCell, Max: cellMax, Running: running,
				Message: fmt.Sprintf("waiting on the tenant cap of cell %s: %d of %d builds run", req.Cell, running, cellMax)}
		}
	}
	return capVerdict{}
}

// checkCaps runs the cap check of one claim inside its transaction. It
// answers the verdict and keeps the wait table in step: a blocked claim
// refreshes its wait row, and a claim that passes the caps deletes it.
// A repeat of a claim that already holds an intent passes at once.
func checkCaps(ctx context.Context, tx pgx.Tx, req ClaimRequest, runID uuid.UUID) (capVerdict, error) {
	var raw []byte
	err := tx.QueryRow(ctx,
		`SELECT policy FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2`,
		req.Cluster, req.Tenant).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return capVerdict{}, clearWaitRow(ctx, tx, runID)
	}
	if err != nil {
		return capVerdict{}, fmt.Errorf("read tenant cap policy: %w", err)
	}
	policy := decodeCapPolicy(raw)
	limits := policy.matching(req.Tenant, req.JobPath)
	if len(limits) == 0 && policy.CellMaxRuns == 0 {
		// A limit that was removed or narrowed must not leave a wait row.
		return capVerdict{}, clearWaitRow(ctx, tx, runID)
	}

	// A repeat of a claim that holds its slot on the same cell passes. A
	// transfer to another cell is checked against that cell, so it cannot
	// exceed the cell cap.
	var heldCell string
	var heldRun uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT run_id, COALESCE(cell, '') FROM varroa.run_intents WHERE intent_key = $1`,
		req.IntentKey).Scan(&heldRun, &heldCell)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return capVerdict{}, fmt.Errorf("read intent for cap check: %w", err)
	}
	if known && heldCell == req.Cell {
		return capVerdict{}, nil
	}

	// The lock serializes the cap checks of one tenant, so two claims
	// cannot both read the same free slot.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('varroa.caps/' || $1 || '/' || $2, 0))`,
		req.Cluster, req.Tenant); err != nil {
		return capVerdict{}, fmt.Errorf("lock tenant caps: %w", err)
	}

	inflight, err := readInFlightRuns(ctx, tx, req)
	if err != nil {
		return capVerdict{}, err
	}
	// The run's own slot does not count against its own claim, and neither
	// does the slot of the intent it replaces on a transfer.
	own := inflight[:0]
	for _, r := range inflight {
		if r.runID != runID && r.runID != heldRun {
			own = append(own, r)
		}
	}
	inflight = own
	waiters, err := readLiveWaiters(ctx, tx, req, runID)
	if err != nil {
		return capVerdict{}, err
	}
	var mine time.Time
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT first_seen_at FROM varroa.run_waits WHERE run_id = $1), now())`,
		runID).Scan(&mine); err != nil {
		return capVerdict{}, fmt.Errorf("read own wait row: %w", err)
	}

	verdict := decideCaps(req.Tenant, req, runID, mine, limits, policy.CellMaxRuns, inflight, waiters)
	if !verdict.Blocked {
		if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_waits WHERE run_id = $1`, runID); err != nil {
			return capVerdict{}, fmt.Errorf("clear wait row: %w", err)
		}
		return verdict, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_waits
			(run_id, cluster, namespace, tenant, job_path, cell, cap_kind, cap_name, cap_max, running)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id) DO UPDATE SET
			namespace = EXCLUDED.namespace, cell = EXCLUDED.cell, cap_kind = EXCLUDED.cap_kind,
			cap_name = EXCLUDED.cap_name, cap_max = EXCLUDED.cap_max, running = EXCLUDED.running,
			last_seen_at = now()`,
		runID, req.Cluster, req.Namespace, req.Tenant, req.JobPath, req.Cell,
		verdict.Kind, verdict.Name, verdict.Max, verdict.Running); err != nil {
		return capVerdict{}, fmt.Errorf("write wait row: %w", err)
	}
	return verdict, nil
}

// readInFlightRuns reads the builds that hold a slot of the tenant: the
// granted intents inside the grant window and the RUNNING runs inside the
// running window. A run in both sets counts once.
func readInFlightRuns(ctx context.Context, tx pgx.Tx, req ClaimRequest) ([]inFlightRun, error) {
	rows, err := tx.Query(ctx, `
		SELECT run_id, job_path, COALESCE(cell, '') FROM varroa.run_intents
		WHERE cluster = $1 AND tenant = $2 AND state = 'GRANTED'
		  AND COALESCE(granted_at, created_at) > now() - make_interval(secs => $3)
		UNION
		SELECT run_id, job_path, COALESCE(cell, '') FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND status = 'RUNNING'
		  AND created_at > now() - make_interval(secs => $4)`,
		req.Cluster, req.Tenant, capGrantWindow.Seconds(), capRunningWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read in-flight runs: %w", err)
	}
	defer rows.Close()
	seen := map[uuid.UUID]bool{}
	var out []inFlightRun
	for rows.Next() {
		var r inFlightRun
		if err := rows.Scan(&r.runID, &r.jobPath, &r.cell); err != nil {
			return nil, fmt.Errorf("scan in-flight run: %w", err)
		}
		if seen[r.runID] {
			continue
		}
		seen[r.runID] = true
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read in-flight runs: %w", err)
	}
	return out, nil
}

// readLiveWaiters reads the wait rows of the tenant that a claim refreshed
// inside the live window, other than the claim's own row.
func readLiveWaiters(ctx context.Context, tx pgx.Tx, req ClaimRequest, runID uuid.UUID) ([]capWaiter, error) {
	rows, err := tx.Query(ctx, `
		SELECT run_id, job_path, cell, cap_kind, cap_name, first_seen_at FROM varroa.run_waits
		WHERE cluster = $1 AND tenant = $2 AND run_id <> $3
		  AND last_seen_at > now() - make_interval(secs => $4)`,
		req.Cluster, req.Tenant, runID, capWaiterLive.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read live waiters: %w", err)
	}
	defer rows.Close()
	var out []capWaiter
	for rows.Next() {
		var w capWaiter
		if err := rows.Scan(&w.runID, &w.jobPath, &w.cell, &w.kind, &w.name, &w.firstSeen); err != nil {
			return nil, fmt.Errorf("scan live waiter: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read live waiters: %w", err)
	}
	return out, nil
}

// clearWaitRow deletes the wait row of a run that no cap holds.
func clearWaitRow(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_waits WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear wait row: %w", err)
	}
	return nil
}
