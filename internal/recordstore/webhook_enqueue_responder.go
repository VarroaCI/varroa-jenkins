package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Tenant agent webhook errors. The responder answers them in the reply body.
const (
	tenantAgentWebhookErrInvalid   = "invalid_request"
	tenantAgentWebhookErrNoWebhook = "no webhook"
	tenantAgentWebhookErrInternal  = "internal"
)

// tenantAgentWebhookMaxBody bounds the message body of one call.
const tenantAgentWebhookMaxBody = 16 << 10

// tenantAgentWebhookTimeout bounds one enqueue's database work.
const tenantAgentWebhookTimeout = 5 * time.Second

// tenantAgentWebhookEvent names the payload of a Tenant agent message.
const tenantAgentWebhookEvent = "tenant_agent.message"

// tenantAgentWebhookKeyPrefix starts the delivery key of a Tenant agent message.
const tenantAgentWebhookKeyPrefix = tenantAgentWebhookEvent + ":"

// tenantAgentWebhookQueueGroup is the queue group the responder subscribes with.
const tenantAgentWebhookQueueGroup = "recorder"

// TenantAgentWebhookRequest asks the recorder to enqueue one message to a
// tenant webhook. The key is session and call: a retried call enqueues once.
type TenantAgentWebhookRequest struct {
	Cluster   string          `json:"cluster"`
	Tenant    string          `json:"tenant"`
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	RunID     string          `json:"run_id,omitempty"`
	Principal string          `json:"principal"`
	Body      json.RawMessage `json:"body"`
}

// TenantAgentWebhookResponse answers one enqueue request.
type TenantAgentWebhookResponse struct {
	OK         bool   `json:"ok"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// tenantAgentWebhookPayload is the JSON body that the tenant webhook receives.
type tenantAgentWebhookPayload struct {
	Event     string          `json:"event"`
	Tenant    string          `json:"tenant"`
	SessionID string          `json:"session_id"`
	Principal string          `json:"principal"`
	RunID     string          `json:"run_id,omitempty"`
	Body      json.RawMessage `json:"body"`
	SentAt    time.Time       `json:"sent_at"`
}

// RegisterWebhookEnqueueResponder answers enqueue requests on the webhook
// subject. nudge runs after a new row commits, so the dispatcher sends it at
// once. A nil nudge does nothing.
func RegisterWebhookEnqueueResponder(conn *bus.Conn, pool *pgxpool.Pool, nudge func(), logger *slog.Logger) (*bus.Responder, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		var req TenantAgentWebhookRequest
		resp := TenantAgentWebhookResponse{}
		if err := json.Unmarshal(data, &req); err != nil {
			resp.Error = tenantAgentWebhookErrInvalid
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), tenantAgentWebhookTimeout)
			defer cancel()
			var err error
			resp, err = EnqueueTenantAgentWebhook(ctx, pool, req, time.Now().UTC())
			if err != nil {
				logger.Error("tenant agent webhook enqueue failed", "session_id", req.SessionID, "error", err)
				resp = TenantAgentWebhookResponse{Error: tenantAgentWebhookErrInternal}
			}
			if resp.OK && nudge != nil && resp.DeliveryID != "" {
				nudge()
			}
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return []byte(`{"ok":false,"error":"internal"}`)
		}
		return out
	}
	return conn.ServeRequests(bus.WebhookEnqueueSubject, tenantAgentWebhookQueueGroup, handler)
}

// EnqueueTenantAgentWebhook writes one outbox row for a Tenant agent message. The
// row carries the incarnation of the tenant placement, so the dispatcher sends
// it to the live tenant. A repeat of the same session and call returns the
// row that already exists and writes nothing more.
func EnqueueTenantAgentWebhook(ctx context.Context, pool *pgxpool.Pool, req TenantAgentWebhookRequest, now time.Time) (TenantAgentWebhookResponse, error) {
	sessionID, err := uuid.Parse(req.SessionID)
	if err != nil || req.Cluster == "" || req.Tenant == "" || req.CallID == "" || req.Principal == "" {
		return TenantAgentWebhookResponse{Error: tenantAgentWebhookErrInvalid}, nil
	}
	if len(req.Body) == 0 || len(req.Body) > tenantAgentWebhookMaxBody || !isJSONObject(req.Body) {
		return TenantAgentWebhookResponse{Error: tenantAgentWebhookErrInvalid}, nil
	}
	// The run id names the run that the call belongs to. A call without a run
	// keys its delivery by the session id, which is a uuid as well.
	runID := sessionID
	runName := ""
	if req.RunID != "" {
		parsed, err := uuid.Parse(req.RunID)
		if err != nil {
			return TenantAgentWebhookResponse{Error: tenantAgentWebhookErrInvalid}, nil
		}
		runID = parsed
		runName = req.RunID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return TenantAgentWebhookResponse{}, fmt.Errorf("begin tenant agent webhook: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hasWebhook bool
	var incarnation string
	err = tx.QueryRow(ctx, `
		SELECT notification_webhook IS NOT NULL, COALESCE(incarnation::text, '')
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
	`, req.Cluster, req.Tenant).Scan(&hasWebhook, &incarnation)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !hasWebhook) {
		return TenantAgentWebhookResponse{Error: tenantAgentWebhookErrNoWebhook}, nil
	}
	if err != nil {
		return TenantAgentWebhookResponse{}, fmt.Errorf("read tenant webhook: %w", err)
	}

	payload, err := json.Marshal(tenantAgentWebhookPayload{
		Event:     tenantAgentWebhookEvent,
		Tenant:    req.Tenant,
		SessionID: req.SessionID,
		Principal: req.Principal,
		RunID:     runName,
		Body:      req.Body,
		SentAt:    now,
	})
	if err != nil {
		return TenantAgentWebhookResponse{}, fmt.Errorf("marshal tenant agent webhook: %w", err)
	}

	deliveryID, err := uuid.NewV7()
	if err != nil {
		return TenantAgentWebhookResponse{}, fmt.Errorf("new delivery id: %w", err)
	}
	key := tenantAgentWebhookKeyPrefix + req.SessionID + ":" + req.CallID
	inserted, err := insertWebhookDelivery(ctx, tx, deliveryID, runID, req.Cluster, req.Tenant, incarnation, key, payload)
	if err != nil {
		return TenantAgentWebhookResponse{}, err
	}
	if !inserted {
		var existing uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT delivery_id FROM varroa.webhook_deliveries
			WHERE run_id = $1 AND delivery_key = $2
		`, runID, key).Scan(&existing); err != nil {
			return TenantAgentWebhookResponse{}, fmt.Errorf("read existing tenant agent delivery: %w", err)
		}
		return TenantAgentWebhookResponse{OK: true, DeliveryID: existing.String()}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return TenantAgentWebhookResponse{}, fmt.Errorf("commit tenant agent webhook: %w", err)
	}
	return TenantAgentWebhookResponse{OK: true, DeliveryID: deliveryID.String()}, nil
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
