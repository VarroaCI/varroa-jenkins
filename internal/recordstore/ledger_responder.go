package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// claimQueueGroup is the NATS queue group the recorder's claim responder
// joins, so more than one recorder replica shares the subscription.
const claimQueueGroup = "recorder"

// claimTimeout bounds one claim's database work, so a stalled Postgres
// connection does not hold the responder goroutine open indefinitely.
const claimTimeout = 10 * time.Second

// RegisterClaimResponder subscribes ledger to bus.LedgerClaimSubject with
// queue group "recorder". The returned Sub stops the subscription when
// unsubscribed.
func RegisterClaimResponder(conn *bus.Conn, ledger *Ledger, logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleClaim(logger, ledger, data)
	}
	return conn.SubscribeRequest(bus.LedgerClaimSubject, claimQueueGroup, handler)
}

// handleClaim decodes a ClaimRequest, runs it against ledger, and encodes
// the ClaimResponse. A body the decoder rejects answers vetoed with
// reason_code=INVALID_KEY, because a request the recorder cannot read
// carries no valid key either.
func handleClaim(logger *slog.Logger, ledger *Ledger, data []byte) []byte {
	var req ClaimRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("claim request decode failed", "error", err)
		resp, _ := json.Marshal(ClaimResponse{
			Decision:   DecisionVetoed,
			ReasonCode: ReasonInvalidKey,
			Message:    "malformed claim request",
		})
		return resp
	}

	ctx, cancel := context.WithTimeout(context.Background(), claimTimeout)
	defer cancel()

	resp := ledger.Claim(ctx, req)
	if resp.Decision == DecisionVetoed {
		// Log the reason code and tenant for a refused claim, once per
		// refusal. Log no other request or response field, so the log
		// never carries the payload body.
		logger.Info("claim refused", "reason_code", resp.ReasonCode, "tenant", req.Tenant)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		logger.Error("claim response encode failed", "error", err)
		out, _ = json.Marshal(ClaimResponse{Code: CodeInternal, Error: "encode claim response"})
	}
	return out
}
