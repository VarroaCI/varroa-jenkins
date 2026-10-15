package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// webhookEventRunCompleted is the X-Varroa-Event value of every completion
// webhook delivery, the owner's and a synthetic end's alike.
const webhookEventRunCompleted = "run.completed"

// webhookKeyCompleted is the delivery_key of the owner's completion row.
const webhookKeyCompleted = "completed"

// webhookKeyInterruptedPrefix starts the delivery_key of a synthetic end.
// The full key is the prefix plus the interrupted reason, so each mark of
// one run owns one row.
const webhookKeyInterruptedPrefix = "interrupted:"

// WebhookPayload is the JSON body of one completion webhook delivery.
// The recorder encodes it once, at enqueue time. The stored bytes are the
// bytes that every attempt signs and sends.
type WebhookPayload struct {
	Event       string     `json:"event"`
	DeliveryID  string     `json:"delivery_id"`
	Cluster     string     `json:"cluster"`
	Tenant      string     `json:"tenant"`
	Job         string     `json:"job"`
	RunID       string     `json:"run_id"`
	Number      int64      `json:"number,omitempty"`
	Result      string     `json:"result"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time  `json:"completed_at"`
	DurationMs  int64      `json:"duration_ms"`
	SCMRevision string     `json:"scm_revision,omitempty"`
	// RejectedSignature names the script signature that the sandbox
	// refused. It is set only for a FAILURE result.
	RejectedSignature string `json:"rejected_signature,omitempty"`
	URL               string `json:"url,omitempty"`
	// Synthetic is true for a delivery the backstop wrote for a run the
	// recorder ended. Its Result is INTERRUPTED.
	Synthetic bool `json:"synthetic,omitempty"`
	// InterruptedReason names the mark of a synthetic delivery.
	InterruptedReason string `json:"interrupted_reason,omitempty"`
	// Supersedes names the delivery_id of the synthetic delivery that a
	// real completion replaces. It is empty when no synthetic delivery
	// preceded the completion.
	Supersedes string `json:"supersedes,omitempty"`
}

// tenantHasWebhook reports whether the tenant declares a completion webhook.
// The outbox writes no row for a tenant that declares none.
func tenantHasWebhook(ctx context.Context, tx pgx.Tx, cluster, tenant string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2 AND notification_webhook IS NOT NULL
	`, cluster, tenant).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read tenant webhook: %w", err)
	}
	return true, nil
}

// insertWebhookDelivery writes one outbox row. It answers false when a row
// with the same run_id and delivery_key already exists.
func insertWebhookDelivery(ctx context.Context, tx pgx.Tx, deliveryID, runID uuid.UUID,
	cluster, tenant, incarnation, key string, body []byte) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.webhook_deliveries (delivery_id, run_id, cluster, tenant, incarnation, body, delivery_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (run_id, delivery_key) DO NOTHING
	`, deliveryID, runID, cluster, tenant, incarnation, body, key)
	if err != nil {
		return false, fmt.Errorf("insert webhook delivery: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// webhookRunURL answers the dashboard link of one run, or an empty string
// when no dashboard origin is set or the tenant name has no link.
func webhookRunURL(dashboardURL, tenant, jobPath, runID string) string {
	if dashboardURL == "" {
		return ""
	}
	rel := crossTenantRunURL(tenant, jobPath, runID)
	if rel == "" {
		return ""
	}
	return strings.TrimSuffix(dashboardURL, "/") + rel
}

// enqueueWebhookDelivery writes one outbox row for the owner run.completed
// that intent describes. It runs in the transaction of that terminal
// write, so a rolled-back completion leaves no row. It answers false, and
// writes nothing, when the tenant declares no webhook. The insert
// conflicts on (run_id, delivery_key), so a redelivered completion
// enqueues nothing.
//
// The row carries the incarnation the claim stamped on the intent. When a
// synthetic delivery already exists for the run, the completion names it
// in supersedes and drops every such delivery that is still pending.
func enqueueWebhookDelivery(ctx context.Context, tx pgx.Tx, intent *intentRow, payload RunCompletedPayload, dashboardURL string) (bool, error) {
	ok, err := tenantHasWebhook(ctx, tx, intent.cluster, intent.tenant)
	if err != nil || !ok {
		return false, err
	}

	var (
		buildNumber *int64
		startedAt   *time.Time
		scmRevision *string
	)
	err = tx.QueryRow(ctx, `
		SELECT build_number, started_at, scm_revision FROM varroa.runs
		WHERE created_at = $1 AND run_id = $2
	`, intent.createdAt, intent.runID).Scan(&buildNumber, &startedAt, &scmRevision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read run for webhook: %w", err)
	}

	deliveryID := uuid.New()
	body := WebhookPayload{
		Event:       webhookEventRunCompleted,
		DeliveryID:  deliveryID.String(),
		Cluster:     intent.cluster,
		Tenant:      intent.tenant,
		Job:         intent.jobPath,
		RunID:       intent.runID.String(),
		Result:      *payload.Result,
		StartedAt:   startedAt,
		CompletedAt: time.Now().UTC(),
	}
	if payload.EndedAt != nil {
		body.CompletedAt = payload.EndedAt.UTC()
	}
	if buildNumber != nil {
		body.Number = *buildNumber
	}
	if payload.DurationMs != nil {
		body.DurationMs = *payload.DurationMs
	}
	switch {
	case scmRevision != nil:
		body.SCMRevision = *scmRevision
	case payload.SCMRevision != nil:
		body.SCMRevision = *payload.SCMRevision
	}
	if body.Result == "FAILURE" && payload.RejectedSignature != nil {
		body.RejectedSignature = *payload.RejectedSignature
	}
	body.URL = webhookRunURL(dashboardURL, intent.tenant, intent.jobPath, intent.runID.String())

	err = tx.QueryRow(ctx, `
		SELECT delivery_id FROM varroa.webhook_deliveries
		WHERE run_id = $1 AND delivery_key <> $2
		ORDER BY created_at DESC LIMIT 1
	`, intent.runID, webhookKeyCompleted).Scan(&body.Supersedes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read synthetic delivery: %w", err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("encode webhook payload: %w", err)
	}

	inserted, err := insertWebhookDelivery(ctx, tx, deliveryID, intent.runID, intent.cluster, intent.tenant,
		intent.incarnation, webhookKeyCompleted, encoded)
	if err != nil || !inserted {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.webhook_deliveries
		SET state = 'dropped', last_error = 'superseded by the run completion', updated_at = now()
		WHERE run_id = $1 AND delivery_key <> $2 AND state = 'pending'
	`, intent.runID, webhookKeyCompleted); err != nil {
		return false, fmt.Errorf("drop superseded synthetic delivery: %w", err)
	}
	return true, nil
}

// enqueueInterruptedWebhookDelivery writes one synthetic outbox row for
// the run that markInterruptedTx has just ended with reason. It runs in
// that transaction. It answers false, and writes nothing, when the tenant
// declares no webhook or when no run_intents row exists: a run that no
// cell claimed has no owning incarnation, so it sends nothing.
func enqueueInterruptedWebhookDelivery(ctx context.Context, tx pgx.Tx, run runInterruptCandidate,
	reason string, now time.Time, dashboardURL string) (bool, error) {
	ok, err := tenantHasWebhook(ctx, tx, run.cluster, run.tenant)
	if err != nil || !ok {
		return false, err
	}

	var incarnation string
	err = tx.QueryRow(ctx, `
		SELECT incarnation FROM varroa.run_intents WHERE run_id = $1
	`, run.runID).Scan(&incarnation)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read intent incarnation: %w", err)
	}

	var (
		buildNumber *int64
		startedAt   *time.Time
		endedAt     *time.Time
		scmRevision *string
	)
	if err := tx.QueryRow(ctx, `
		SELECT build_number, started_at, ended_at, scm_revision FROM varroa.runs
		WHERE created_at = $1 AND run_id = $2
	`, run.createdAt, run.runID).Scan(&buildNumber, &startedAt, &endedAt, &scmRevision); err != nil {
		return false, fmt.Errorf("read run for synthetic webhook: %w", err)
	}

	deliveryID := uuid.New()
	body := WebhookPayload{
		Event:             webhookEventRunCompleted,
		DeliveryID:        deliveryID.String(),
		Cluster:           run.cluster,
		Tenant:            run.tenant,
		Job:               run.jobPath,
		RunID:             run.runID.String(),
		Result:            "INTERRUPTED",
		StartedAt:         startedAt,
		CompletedAt:       now.UTC(),
		Synthetic:         true,
		InterruptedReason: reason,
	}
	if endedAt != nil {
		body.CompletedAt = endedAt.UTC()
	}
	if startedAt != nil {
		body.DurationMs = body.CompletedAt.Sub(*startedAt).Milliseconds()
	}
	if buildNumber != nil {
		body.Number = *buildNumber
	}
	if scmRevision != nil {
		body.SCMRevision = *scmRevision
	}
	body.URL = webhookRunURL(dashboardURL, run.tenant, run.jobPath, run.runID.String())
	encoded, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("encode synthetic webhook payload: %w", err)
	}

	return insertWebhookDelivery(ctx, tx, deliveryID, run.runID, run.cluster, run.tenant,
		incarnation, webhookKeyInterruptedPrefix+reason, encoded)
}
