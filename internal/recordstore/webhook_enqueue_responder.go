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

// Team agent webhook errors. The responder answers them in the reply body.
const (
	teamAgentWebhookErrInvalid   = "invalid_request"
	teamAgentWebhookErrNoWebhook = "no webhook"
	teamAgentWebhookErrInternal  = "internal"
)

// teamAgentWebhookMaxBody bounds the message body of one call.
const teamAgentWebhookMaxBody = 16 << 10

// teamAgentWebhookTimeout bounds one enqueue's database work.
const teamAgentWebhookTimeout = 5 * time.Second

// teamAgentWebhookEvent names the payload of a Team agent message.
const teamAgentWebhookEvent = "team_agent.message"

// teamAgentWebhookKeyPrefix starts the delivery key of a Team agent message.
const teamAgentWebhookKeyPrefix = teamAgentWebhookEvent + ":"

// teamAgentWebhookQueueGroup is the queue group the responder subscribes with.
const teamAgentWebhookQueueGroup = "recorder"

// TeamAgentWebhookRequest asks the recorder to enqueue one message to a
// tenant webhook. The key is session and call: a retried call enqueues once.
type TeamAgentWebhookRequest struct {
	Cluster   string          `json:"cluster"`
	Tenant    string          `json:"tenant"`
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	RunID     string          `json:"run_id,omitempty"`
	Principal string          `json:"principal"`
	Body      json.RawMessage `json:"body"`
}

// TeamAgentWebhookResponse answers one enqueue request.
type TeamAgentWebhookResponse struct {
	OK         bool   `json:"ok"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// teamAgentWebhookPayload is the JSON body that the tenant webhook receives.
type teamAgentWebhookPayload struct {
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
		var req TeamAgentWebhookRequest
		resp := TeamAgentWebhookResponse{}
		if err := json.Unmarshal(data, &req); err != nil {
			resp.Error = teamAgentWebhookErrInvalid
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), teamAgentWebhookTimeout)
			defer cancel()
			var err error
			resp, err = EnqueueTeamAgentWebhook(ctx, pool, req, time.Now().UTC())
			if err != nil {
				logger.Error("team agent webhook enqueue failed", "session_id", req.SessionID, "error", err)
				resp = TeamAgentWebhookResponse{Error: teamAgentWebhookErrInternal}
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
	return conn.ServeRequests(bus.WebhookEnqueueSubject, teamAgentWebhookQueueGroup, handler)
}

// EnqueueTeamAgentWebhook writes one outbox row for a Team agent message. The
// row carries the incarnation of the tenant placement, so the dispatcher sends
// it to the live tenant. A repeat of the same session and call returns the
// row that already exists and writes nothing more.
func EnqueueTeamAgentWebhook(ctx context.Context, pool *pgxpool.Pool, req TeamAgentWebhookRequest, now time.Time) (TeamAgentWebhookResponse, error) {
	sessionID, err := uuid.Parse(req.SessionID)
	if err != nil || req.Cluster == "" || req.Tenant == "" || req.CallID == "" || req.Principal == "" {
		return TeamAgentWebhookResponse{Error: teamAgentWebhookErrInvalid}, nil
	}
	if len(req.Body) == 0 || len(req.Body) > teamAgentWebhookMaxBody || !isJSONObject(req.Body) {
		return TeamAgentWebhookResponse{Error: teamAgentWebhookErrInvalid}, nil
	}
	// The run id names the run that the call belongs to. A call without a run
	// keys its delivery by the session id, which is a uuid as well.
	runID := sessionID
	runName := ""
	if req.RunID != "" {
		parsed, err := uuid.Parse(req.RunID)
		if err != nil {
			return TeamAgentWebhookResponse{Error: teamAgentWebhookErrInvalid}, nil
		}
		runID = parsed
		runName = req.RunID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return TeamAgentWebhookResponse{}, fmt.Errorf("begin team agent webhook: %w", err)
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
		return TeamAgentWebhookResponse{Error: teamAgentWebhookErrNoWebhook}, nil
	}
	if err != nil {
		return TeamAgentWebhookResponse{}, fmt.Errorf("read tenant webhook: %w", err)
	}

	payload, err := json.Marshal(teamAgentWebhookPayload{
		Event:     teamAgentWebhookEvent,
		Tenant:    req.Tenant,
		SessionID: req.SessionID,
		Principal: req.Principal,
		RunID:     runName,
		Body:      req.Body,
		SentAt:    now,
	})
	if err != nil {
		return TeamAgentWebhookResponse{}, fmt.Errorf("marshal team agent webhook: %w", err)
	}

	deliveryID, err := uuid.NewV7()
	if err != nil {
		return TeamAgentWebhookResponse{}, fmt.Errorf("new delivery id: %w", err)
	}
	key := teamAgentWebhookKeyPrefix + req.SessionID + ":" + req.CallID
	inserted, err := insertWebhookDelivery(ctx, tx, deliveryID, runID, req.Cluster, req.Tenant, incarnation, key, payload)
	if err != nil {
		return TeamAgentWebhookResponse{}, err
	}
	if !inserted {
		var existing uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT delivery_id FROM varroa.webhook_deliveries
			WHERE run_id = $1 AND delivery_key = $2
		`, runID, key).Scan(&existing); err != nil {
			return TeamAgentWebhookResponse{}, fmt.Errorf("read existing team agent delivery: %w", err)
		}
		return TeamAgentWebhookResponse{OK: true, DeliveryID: existing.String()}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamAgentWebhookResponse{}, fmt.Errorf("commit team agent webhook: %w", err)
	}
	return TeamAgentWebhookResponse{OK: true, DeliveryID: deliveryID.String()}, nil
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
