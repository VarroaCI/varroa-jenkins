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

// webhookEventRunCompleted is the X-Varroa-Event value of a completion
// webhook delivery.
const webhookEventRunCompleted = "run.completed"

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
	URL         string     `json:"url,omitempty"`
}

// enqueueWebhookDelivery writes one outbox row for the owner run.completed
// that intent describes. It runs in the transaction of that terminal
// write, so a rolled-back completion leaves no row. It answers false, and
// writes nothing, when the tenant declares no webhook. The insert
// conflicts on run_id, so a redelivered completion enqueues nothing.
func enqueueWebhookDelivery(ctx context.Context, tx pgx.Tx, intent *intentRow, payload RunCompletedPayload, dashboardURL string) (bool, error) {
	var incarnation string
	err := tx.QueryRow(ctx, `
		SELECT incarnation FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2 AND notification_webhook IS NOT NULL
	`, intent.cluster, intent.tenant).Scan(&incarnation)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read tenant webhook: %w", err)
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
	if dashboardURL != "" {
		if rel := crossTenantRunURL(intent.tenant, intent.jobPath, intent.runID.String()); rel != "" {
			body.URL = strings.TrimSuffix(dashboardURL, "/") + rel
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("encode webhook payload: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.webhook_deliveries (delivery_id, run_id, cluster, tenant, incarnation, body)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id) DO NOTHING
	`, deliveryID, intent.runID, intent.cluster, intent.tenant, incarnation, encoded)
	if err != nil {
		return false, fmt.Errorf("insert webhook delivery: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
