package mite

import (
	"context"
	"encoding/json"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// crossTenantMaxInFlight bounds concurrent cross-tenant requests per
// connection (design.md D3), so a slow record store cannot exhaust the
// gateway on one controller's behalf.
const crossTenantMaxInFlight = 8

// codeInternal is the CrossTenantResponse failure code for a fault other
// than an unreachable record store (design.md D4). codeUnavailable
// already covers the "did not answer" case for the claim path and is
// reused here.
const codeInternal = "INTERNAL"

// unhostedCrossTenantCallerMessage is the error text a cross-tenant
// request receives when its caller_job_path's first segment names no
// tenant the cell hosts.
const unhostedCrossTenantCallerMessage = "caller job path names a tenant this cell does not host"

// crossTenantKinds is the closed set of cross-tenant request kinds
// (design.md D4). The gateway rejects any other kind before it ever
// reaches the record store.
var crossTenantKinds = map[string]bool{
	"build":           true,
	"status":          true,
	"last_successful": true,
	"artifact_list":   true,
	"artifact_url":    true,
	"lock_acquire":    true,
	"lock_renew":      true,
	"lock_release":    true,
}

// crossTenantErrorPayload is the payload_json body of a CrossTenantResponse
// the gateway builds itself, without a record-store round trip.
type crossTenantErrorPayload struct {
	Kind  string `json:"kind,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// crossTenantKey identifies one connection's in-flight cross-tenant
// counter. Shares claimKey's format ("ns/name#epoch") since both counters
// are scoped by connection epoch, not by controller alone, for the same
// reason: a goroutine that finishes after its stream ended must never
// lower a replacement stream's count.
func crossTenantKey(ns, name string, connToken int64) string {
	return claimKey(ns, name, connToken)
}

// tryReserveCrossTenantSlot reserves one in-flight cross-tenant slot for
// this connection, up to crossTenantMaxInFlight. It reports false when
// the cap is already reached.
func (h *BusHandler) tryReserveCrossTenantSlot(ns, name string, connToken int64) bool {
	key := crossTenantKey(ns, name, connToken)
	h.crossTenantMu.Lock()
	defer h.crossTenantMu.Unlock()
	if h.crossTenantCounts == nil {
		h.crossTenantCounts = make(map[string]int)
	}
	if h.crossTenantCounts[key] >= crossTenantMaxInFlight {
		return false
	}
	h.crossTenantCounts[key]++
	return true
}

// releaseCrossTenantSlot releases one in-flight cross-tenant slot
// reserved by tryReserveCrossTenantSlot. Called in a defer from the
// request's own goroutine, so every outcome releases it.
func (h *BusHandler) releaseCrossTenantSlot(ns, name string, connToken int64) {
	key := crossTenantKey(ns, name, connToken)
	h.crossTenantMu.Lock()
	defer h.crossTenantMu.Unlock()
	if h.crossTenantCounts[key] > 0 {
		h.crossTenantCounts[key]--
		if h.crossTenantCounts[key] == 0 {
			delete(h.crossTenantCounts, key)
		}
	}
}

// clearCrossTenantSlots deletes the in-flight counter for connToken. A
// late release from a goroutine of that same connection then finds the
// counter already gone and changes nothing, so it can never push a live
// counter below zero.
func (h *BusHandler) clearCrossTenantSlots(ns, name string, connToken int64) {
	key := crossTenantKey(ns, name, connToken)
	h.crossTenantMu.Lock()
	delete(h.crossTenantCounts, key)
	h.crossTenantMu.Unlock()
}

// respondCrossTenantError answers a cross-tenant request with a
// gateway-built payload the record store never saw, echoing request_id
// and kind.
func (h *BusHandler) respondCrossTenantError(send Sender, req *mitev1.CrossTenantRequest, code, message string) {
	body, err := json.Marshal(crossTenantErrorPayload{Kind: req.Kind, Error: message, Code: code})
	if err != nil {
		// A fixed-shape struct of three strings cannot fail to encode;
		// fall back to a literal body rather than send nothing.
		body = []byte(`{"error":"encode cross-tenant error response","code":"` + codeInternal + `"}`)
	}
	resp := &mitev1.CrossTenantResponse{
		RequestId:   req.RequestId,
		Kind:        req.Kind,
		PayloadJson: string(body),
	}
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_CrossTenantResponse{CrossTenantResponse: resp},
	}
	if err := send(msg); err != nil {
		h.log().Error("send cross-tenant error response failed", "kind", req.Kind, "error", err)
	}
}

// OnCrossTenantRequest bridges one CrossTenantRequest to the record
// store's cross-tenant subject (design.md D3, D4). It rejects an unknown
// kind and reserves an in-flight slot synchronously, so neither case ever
// starts a goroutine or calls NATS, then runs the round trip in its own
// goroutine so a slow record store cannot block this connection's
// CommandStream read loop.
func (h *BusHandler) OnCrossTenantRequest(name, ns string, connToken int64, req *mitev1.CrossTenantRequest, send Sender) {
	if !crossTenantKinds[req.Kind] {
		h.respondCrossTenantError(send, req, codeInternal, "unknown cross-tenant request kind")
		return
	}
	if !h.tryReserveCrossTenantSlot(ns, name, connToken) {
		h.respondCrossTenantError(send, req, codeUnavailable, "too many in-flight cross-tenant requests for this connection")
		return
	}
	go func() {
		defer h.releaseCrossTenantSlot(ns, name, connToken)
		h.handleCrossTenantRequest(name, ns, req, send)
	}()
}

// handleCrossTenantRequest decodes the payload, resolves and overwrites
// the identity fields, makes the request-reply call, and forwards the
// reply (or a failure) to the mite. The reply is copied through verbatim:
// design.md D3 makes the gateway a relay for a contract the record store
// owns, so this package parses only the fields it needs to route and
// authorize the request, never the reply.
func (h *BusHandler) handleCrossTenantRequest(name, ns string, req *mitev1.CrossTenantRequest, send Sender) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(req.PayloadJson), &payload); err != nil {
		h.respondCrossTenantError(send, req, codeInternal, "malformed cross-tenant payload: "+err.Error())
		return
	}

	callerJobPath, _ := payload["caller_job_path"].(string)
	tenant, placementGeneration := h.resolver().ResolveJobTenant(h.cluster, ns, name, callerJobPath)
	if tenant == "" {
		// A cell must not present a caller tenant it does not host. Fail
		// closed with no request at all, exactly as the claim path does
		// for an unhosted job path (design.md D3).
		h.respondCrossTenantError(send, req, codeUnavailable, unhostedCrossTenantCallerMessage)
		return
	}

	// Overwrite the identity block, whatever the cell sent (design.md
	// D3, D4). kind is set from the proto field validated in
	// OnCrossTenantRequest, not from whatever the payload itself claims.
	// requested_at is stamped from the gateway's own clock, so a caller
	// cannot supply an arbitrary request time.
	payload["cluster"] = h.cluster
	payload["namespace"] = ns
	payload["cell"] = name
	payload["caller_tenant"] = tenant
	payload["placement_generation"] = placementGeneration
	payload["kind"] = req.Kind
	payload["requested_at"] = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.Marshal(payload)
	if err != nil {
		h.respondCrossTenantError(send, req, codeInternal, "encode cross-tenant request: "+err.Error())
		return
	}

	msg, err := h.ledger.RequestWithContext(context.Background(), bus.LedgerCrossTenantSubject, data, h.crossTenantGatewayTimeout())
	if err != nil {
		h.respondCrossTenantError(send, req, codeUnavailable, "cross-tenant ledger request failed: "+err.Error())
		return
	}

	resp := &mitev1.CrossTenantResponse{
		RequestId:   req.RequestId,
		Kind:        req.Kind,
		PayloadJson: string(msg.Data),
	}
	out := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_CrossTenantResponse{CrossTenantResponse: resp},
	}
	if err := send(out); err != nil {
		h.log().Error("send cross-tenant response failed", "controller", ns+"/"+name, "kind", req.Kind, "error", err)
	}
}
