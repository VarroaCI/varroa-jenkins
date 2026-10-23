package recordstore

import (
	"context"
	"fmt"
	"time"
)

// agentRetentionBatch caps the sessions one statement deletes.
const agentRetentionBatch = 1000

// agentRetentionRoundBudget caps the sessions the agent step deletes in
// one maintenance round.
const agentRetentionRoundBudget = 10000

// agentFinalStates lists the states that end a session. A waiting or
// running session is never deleted.
const agentFinalStates = "'completed','failed','cancelled','skipped','expired'"

// deleteExpiredAgentSessionsSQL deletes one batch of final agent sessions
// with their events and approvals in one statement. The optional cluster
// and tenant filters narrow the batch to one tenant.
const deleteExpiredAgentSessionsSQL = `
WITH doomed AS (
  SELECT id FROM varroa.agent_sessions
  WHERE state IN (` + agentFinalStates + `)
    AND COALESCE(ended_at, started_at) < $1
    %s
  ORDER BY COALESCE(ended_at, started_at)
  LIMIT $2
  FOR UPDATE SKIP LOCKED),
ev AS (DELETE FROM varroa.agent_session_events WHERE session_id IN (SELECT id FROM doomed)),
ap AS (DELETE FROM varroa.agent_approvals WHERE session_id IN (SELECT id FROM doomed))
DELETE FROM varroa.agent_sessions WHERE id IN (SELECT id FROM doomed)`

// tenantFilterSQL narrows a batch to one tenant. Its placeholders are $3
// and $4.
const tenantFilterSQL = `AND cluster = $3 AND tenant = $4`

// deleteExpiredAgentSessions deletes final agent sessions past their
// window, with their events and approvals. The store-wide window applies
// to every session. A tenant window shorter than the store-wide window
// applies to that tenant's sessions only. It deletes at most budget
// sessions and returns the number it deleted.
//
//nolint:unparam // the budget stays a parameter so the round bound is visible at the call site
func deleteExpiredAgentSessions(ctx context.Context, db maintDB, now time.Time, storeRuns time.Duration, budget int) (int, error) {
	if storeRuns <= 0 {
		return 0, nil
	}
	rows, err := db.Query(ctx, `SELECT cluster, tenant, policy FROM varroa.tenant_placements`)
	if err != nil {
		return 0, fmt.Errorf("select tenant agent windows: %w", err)
	}
	var tenants []tenantPolicyRow
	for rows.Next() {
		var row tenantPolicyRow
		var raw []byte
		if err := rows.Scan(&row.cluster, &row.tenant, &raw); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan tenant agent window: %w", err)
		}
		row.policy = decodeRetention(raw)
		if row.policy.Runs > 0 && row.policy.Runs < storeRuns {
			tenants = append(tenants, row)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("select tenant agent windows: %w", err)
	}
	rows.Close()

	total := 0
	storeSQL := fmt.Sprintf(deleteExpiredAgentSessionsSQL, "")
	n, err := deleteAgentBatches(ctx, db, storeSQL, now.Add(-storeRuns), nil, budget)
	total += n
	if err != nil {
		return total, err
	}
	for _, t := range tenants {
		if total >= budget {
			break
		}
		tenantSQL := fmt.Sprintf(deleteExpiredAgentSessionsSQL, tenantFilterSQL)
		cutoff := now.Add(-t.policy.Runs)
		n, err := deleteAgentBatches(ctx, db, tenantSQL, cutoff, &[2]string{t.cluster, t.tenant}, budget-total)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// deleteAgentBatches runs batches of sql until one batch deletes fewer
// than agentRetentionBatch sessions or the budget is used.
func deleteAgentBatches(ctx context.Context, db maintDB, sql string, cutoff time.Time, tenant *[2]string, budget int) (int, error) {
	total := 0
	for total < budget {
		limit := agentRetentionBatch
		if rest := budget - total; rest < limit {
			limit = rest
		}
		args := []any{cutoff, limit}
		if tenant != nil {
			args = append(args, tenant[0], tenant[1])
		}
		tag, err := db.Exec(ctx, sql, args...)
		if err != nil {
			return total, fmt.Errorf("delete expired agent sessions: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < agentRetentionBatch {
			break
		}
	}
	return total, nil
}
