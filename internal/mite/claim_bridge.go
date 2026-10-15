package mite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// maxInFlightClaims bounds concurrent ledger claim requests per
// connection, so a slow record store cannot exhaust the gateway on one
// controller's behalf.
const maxInFlightClaims = 32

// unhostedTenantMessage is the ClaimResponse error text a cell claim
// receives when its job path's first segment names no tenant the cell
// hosts.
const unhostedTenantMessage = "job path names a tenant this cell does not host"

// codeUnavailable is the ClaimResponse failure code C1 defines for an
// unreachable or unparsable record store. C2 defines no new code.
const codeUnavailable = "UNAVAILABLE"

// claimDecisionGranted is the ledger's ClaimResponse.Decision value for a
// grant, mirroring internal/recordstore.DecisionGranted's literal value.
const claimDecisionGranted = "granted"

// ledgerClaimRequest is the request body of a varroa.ledger.claim call.
// Field names and JSON tags mirror internal/recordstore's ClaimRequest
// exactly; the gateway and the recorder communicate over the wire only.
// Origin is copied from the mite's request as it is: the recorder marks a
// vetoed run only when the origin is VARROA_INTENT.
type ledgerClaimRequest struct {
	IntentKey              string    `json:"intent_key"`
	Origin                 string    `json:"origin"`
	RunID                  string    `json:"run_id"`
	Cluster                string    `json:"cluster"`
	Namespace              string    `json:"namespace"`
	Cell                   string    `json:"cell"`
	CellInstance           string    `json:"cell_instance"`
	Tenant                 string    `json:"tenant"`
	JobPath                string    `json:"job_path"`
	Branch                 string    `json:"branch"`
	Actor                  string    `json:"actor"`
	Cause                  string    `json:"cause"`
	ParamsDigest           string    `json:"params_digest"`
	PlacementGeneration    int64     `json:"placement_generation"`
	JenkinsNextBuildNumber int64     `json:"jenkins_next_build_number"`
	RequestedAt            time.Time `json:"requested_at"`
	NumberConflict         int64     `json:"number_conflict,omitempty"`
	StartMark              bool      `json:"start_mark,omitempty"`
}

// ledgerClaimResponse is the reply body of a varroa.ledger.claim call.
type ledgerClaimResponse struct {
	Decision    string `json:"decision,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	BuildNumber int64  `json:"build_number,omitempty"`
	ReasonCode  string `json:"reason_code,omitempty"`
	Message     string `json:"message,omitempty"`
	HomeSlot    *int   `json:"home_slot,omitempty"`
	Error       string `json:"error,omitempty"`
	Code        string `json:"code,omitempty"`
}

// CellTenantResolver resolves the ledger tenant and placement generation
// a claim or a job-schedule row carries. The default resolver returns the
// single-tenant placeholder "<namespace>/<cell>" and generation 0, which
// is the permanent contract for a controller with no cell role. A later
// change replaces the resolver with one that reads the Team custom
// resource.
type CellTenantResolver interface {
	ResolveTenant(cluster, ns, cell string) (tenant string, placementGeneration int64)

	// ResolveJobTenant resolves the tenant and placement generation of one
	// job path within a cell, for one row of a job.schedules envelope. A
	// cell hosts many tenants, so this differs from ResolveTenant by job
	// path rather than by cell alone. The default resolver maps every job
	// path to the same single-tenant placeholder ResolveTenant returns; a
	// later, placement-aware resolver differs per job path. An empty
	// returned tenant means the cell hosts no tenant for that job path.
	ResolveJobTenant(cluster, ns, cell, jobPath string) (tenant string, placementGeneration int64)
}

// defaultCellTenantResolver is the single-tenant placeholder resolver.
type defaultCellTenantResolver struct{}

func (defaultCellTenantResolver) ResolveTenant(_, ns, cell string) (string, int64) {
	return ns + "/" + cell, 0
}

func (defaultCellTenantResolver) ResolveJobTenant(_, ns, cell, _ string) (string, int64) {
	return ns + "/" + cell, 0
}

// SetCellTenantResolver replaces the tenant resolver a claim's ledger
// request body uses. Tests and a later placement-aware caller use this;
// the default is the single-tenant placeholder.
func (h *BusHandler) SetCellTenantResolver(r CellTenantResolver) {
	if r == nil {
		r = defaultCellTenantResolver{}
	}
	h.tenantResolver = r
}

func (h *BusHandler) resolver() CellTenantResolver {
	if h.tenantResolver == nil {
		return defaultCellTenantResolver{}
	}
	return h.tenantResolver
}

const defaultLedgerClaimTimeout = 4 * time.Second

// ledgerClaimTimeout reads VARROA_LEDGER_TIMEOUT_MS (design.md D6), the
// gateway's own hop of the shrink-inward claim timeout budget.
func ledgerClaimTimeout() time.Duration {
	v := os.Getenv("VARROA_LEDGER_TIMEOUT_MS")
	if v == "" {
		return defaultLedgerClaimTimeout
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return defaultLedgerClaimTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// claimKey identifies one connection's in-flight claim counter. It is
// scoped by connToken (the connection epoch), not by controller alone,
// so a goroutine that finishes after its stream ended can never lower a
// replacement stream's count.
func claimKey(ns, name string, connToken int64) string {
	return fmt.Sprintf("%s/%s#%d", ns, name, connToken)
}

// tryReserveClaimSlot reserves one in-flight claim slot for this
// connection, up to maxInFlightClaims. It reports false when the cap is
// already reached.
func (h *BusHandler) tryReserveClaimSlot(ns, name string, connToken int64) bool {
	key := claimKey(ns, name, connToken)
	h.claimMu.Lock()
	defer h.claimMu.Unlock()
	if h.claimCounts == nil {
		h.claimCounts = make(map[string]int)
	}
	if h.claimCounts[key] >= maxInFlightClaims {
		return false
	}
	h.claimCounts[key]++
	return true
}

// releaseClaimSlot releases one in-flight claim slot reserved by
// tryReserveClaimSlot. Called in a defer from the claim's own goroutine,
// so every outcome releases it.
func (h *BusHandler) releaseClaimSlot(ns, name string, connToken int64) {
	key := claimKey(ns, name, connToken)
	h.claimMu.Lock()
	defer h.claimMu.Unlock()
	if h.claimCounts[key] > 0 {
		h.claimCounts[key]--
		if h.claimCounts[key] == 0 {
			delete(h.claimCounts, key)
		}
	}
}

// clearClaimSlots deletes the in-flight counter for connToken. A late
// release from a goroutine of that same connection then finds the
// counter already gone and changes nothing, so it can never push a live
// counter below zero.
func (h *BusHandler) clearClaimSlots(ns, name string, connToken int64) {
	key := claimKey(ns, name, connToken)
	h.claimMu.Lock()
	delete(h.claimCounts, key)
	h.claimMu.Unlock()
}

// respondClaimUnavailable answers a claim with an empty decision and the
// UNAVAILABLE failure code, echoing requestID. It logs each answer at
// Warn, because the plugin holds the build and retries, and no other
// component records the failure.
func (h *BusHandler) respondClaimUnavailable(send Sender, ns, name, requestID, errMsg string) {
	h.log().Warn("claim answered unavailable",
		"request_id", requestID,
		"controller", ns+"/"+name,
		"error", errMsg)
	resp := &mitev1.ClaimResponse{
		RequestId: requestID,
		Code:      codeUnavailable,
		Error:     errMsg,
	}
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_ClaimResponse{ClaimResponse: resp},
	}
	if err := send(msg); err != nil {
		h.log().Error("send claim unavailable response failed", "request_id", requestID, "error", err)
	}
}

// OnClaimRequest bridges one ClaimRequest to the ledger's request-reply
// subject. It reserves an in-flight slot synchronously (so an over-cap
// request never starts a goroutine or calls NATS), then runs the ledger
// round trip in its own goroutine so a slow record store cannot block
// this connection's CommandStream read loop.
func (h *BusHandler) OnClaimRequest(name, ns string, connToken int64, req *mitev1.ClaimRequest, send Sender) {
	if !h.tryReserveClaimSlot(ns, name, connToken) {
		h.respondClaimUnavailable(send, ns, name, req.RequestId, "too many in-flight claims for this connection")
		return
	}
	go func() {
		defer h.releaseClaimSlot(ns, name, connToken)
		h.handleClaimRequest(name, ns, req, send)
	}()
}

// handleClaimRequest builds the ledger request body, makes the
// request-reply call, and forwards the reply (or a failure) to the mite.
func (h *BusHandler) handleClaimRequest(name, ns string, req *mitev1.ClaimRequest, send Sender) {
	// Resolved per job path, not per cell: one cell hosts many tenants,
	// and design.md D14 fences a claim to the tenant that job path names.
	tenant, placementGeneration := h.resolver().ResolveJobTenant(h.cluster, ns, name, req.JobPath)
	if tenant == "" {
		// A cell must not write a ledger claim under a tenant it does not
		// host. Fail closed with the record store's own unavailable code
		// and no request at all — the queue then holds the build, which
		// is the correct outcome for a job the cell should not run.
		h.metrics.claimsRejected.Add(context.Background(), 1)
		h.respondClaimUnavailable(send, ns, name, req.RequestId, unhostedTenantMessage)
		return
	}

	body := ledgerClaimRequest{
		IntentKey:              req.IntentKey,
		Origin:                 req.Origin,
		RunID:                  req.RunId,
		Cluster:                h.cluster,
		Namespace:              ns,
		Cell:                   name,
		CellInstance:           req.CellInstance,
		Tenant:                 tenant,
		JobPath:                req.JobPath,
		Branch:                 req.Branch,
		Actor:                  req.Actor,
		Cause:                  req.Cause,
		ParamsDigest:           req.ParamsDigest,
		PlacementGeneration:    placementGeneration,
		JenkinsNextBuildNumber: req.JenkinsNextBuildNumber,
		RequestedAt:            millisToTime(req.RequestedAtMillis),
		NumberConflict:         req.NumberConflict,
		StartMark:              req.StartMark,
	}

	data, err := json.Marshal(body)
	if err != nil {
		h.respondClaimUnavailable(send, ns, name, req.RequestId, "encode ledger claim request: "+err.Error())
		return
	}

	timeout := ledgerClaimTimeout()
	msg, err := h.ledger.RequestWithContext(context.Background(), bus.LedgerClaimSubject, data, timeout)
	if err != nil {
		h.respondClaimUnavailable(send, ns, name, req.RequestId, "ledger claim request failed: "+err.Error())
		return
	}

	var reply ledgerClaimResponse
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		h.respondClaimUnavailable(send, ns, name, req.RequestId, "ledger claim reply unparsable: "+err.Error())
		return
	}

	// A granted start mark moves no owner: the claim that granted the run
	// already filled the cache.
	if reply.Decision == claimDecisionGranted && reply.RunID != "" && h.ownerCache != nil && !req.StartMark {
		// Fills the run-owner cache at no lookup cost, and invalidates a
		// stale match unconditionally on an ownership transfer (design.md
		// D4): the gateway already brokers this claim, so relaying it is
		// free.
		h.ownerCache.observeGrant(reply.RunID, h.cluster, ns, name, req.CellInstance)
	}

	resp := &mitev1.ClaimResponse{
		RequestId:   req.RequestId,
		Decision:    reply.Decision,
		RunId:       reply.RunID,
		BuildNumber: reply.BuildNumber,
		ReasonCode:  reply.ReasonCode,
		Message:     reply.Message,
		Error:       reply.Error,
		Code:        reply.Code,
		HomeSlot:    reply.HomeSlot,
	}
	out := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_ClaimResponse{ClaimResponse: resp},
	}
	if err := send(out); err != nil {
		h.log().Error("send claim response failed", "controller", ns+"/"+name, "request_id", req.RequestId, "error", err)
	}
}
