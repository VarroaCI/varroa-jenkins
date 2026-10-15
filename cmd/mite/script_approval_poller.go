package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	// scriptApprovalsPath is the plugin route the mite polls for the
	// script signatures the sandbox refused and no operator approved.
	scriptApprovalsPath = "/varroa-activity/script-approvals"

	// scriptApprovalPollInterval is the wait between two polls on a cell.
	scriptApprovalPollInterval = time.Minute
)

// pendingRequesterRow is one requester of the plugin's script-approvals
// response.
type pendingRequesterRow struct {
	Tenant      string `json:"tenant"`
	JobPath     string `json:"jobPath"`
	BuildNumber int64  `json:"buildNumber"`
	RunID       string `json:"runId"`
}

// pendingSignatureRow is one row of the plugin's script-approvals response.
type pendingSignatureRow struct {
	Signature  string                `json:"signature"`
	Dangerous  bool                  `json:"dangerous"`
	Requesters []pendingRequesterRow `json:"requesters"`
}

// scriptApprovalResponse is the JSON envelope the script-approvals endpoint
// returns.
type scriptApprovalResponse struct {
	Pending   []pendingSignatureRow `json:"pending"`
	Total     int                   `json:"total"`
	Truncated bool                  `json:"truncated"`
}

// startScriptApprovalPoller polls the plugin's script-approvals endpoint
// until ctx ends. It polls only while the tenant gate reports a cell. A
// failed poll keeps the last set. A controller that is not a cell clears
// the cached set, so a stale list never rides a snapshot.
func (a *Agent) startScriptApprovalPoller(ctx context.Context) {
	logger := a.Logger.With("component", "script-approval-poller")

	interval := a.scriptApprovalInterval
	if interval <= 0 {
		interval = scriptApprovalPollInterval
	}
	for {
		if a.tenantGateIsCell() {
			set, err := a.pollScriptApprovals(ctx)
			if err != nil {
				logger.Warn("script-approval poll failed, keeping last known set", "error", err)
			} else {
				a.storeScriptApprovalSet(set)
			}
		} else {
			a.storeScriptApprovalSet(nil)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// tenantGateIsCell reads the cell flag of the newest applied desired state.
func (a *Agent) tenantGateIsCell() bool {
	a.tenantGateMu.Lock()
	defer a.tenantGateMu.Unlock()
	return a.tenantGateCell
}

// storeScriptApprovalSet replaces the cached pending set.
func (a *Agent) storeScriptApprovalSet(set *mitev1.PendingSignatureSet) {
	a.scriptApprovalMu.Lock()
	a.scriptApprovalSet = set
	a.scriptApprovalMu.Unlock()
}

// pollScriptApprovals performs one GET against the script-approvals route
// and converts the response into a PendingSignatureSet.
func (a *Agent) pollScriptApprovals(ctx context.Context) (*mitev1.PendingSignatureSet, error) {
	httpResp, err := a.getJenkinsClient().DoGet(ctx, scriptApprovalsPath)
	if err != nil {
		return nil, fmt.Errorf("do get: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
	}

	var resp scriptApprovalResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return toPendingSignatureSet(resp), nil
}

// toPendingSignatureSet converts one plugin response into the wire set.
func toPendingSignatureSet(resp scriptApprovalResponse) *mitev1.PendingSignatureSet {
	items := make([]*mitev1.PendingSignature, 0, len(resp.Pending))
	for _, row := range resp.Pending {
		requesters := make([]*mitev1.PendingSignatureRequester, 0, len(row.Requesters))
		for _, r := range row.Requesters {
			requesters = append(requesters, &mitev1.PendingSignatureRequester{
				Tenant:      r.Tenant,
				JobPath:     r.JobPath,
				BuildNumber: r.BuildNumber,
				RunId:       r.RunID,
			})
		}
		items = append(items, &mitev1.PendingSignature{
			Signature:  row.Signature,
			Dangerous:  row.Dangerous,
			Requesters: requesters,
		})
	}
	return &mitev1.PendingSignatureSet{
		Items:     items,
		Total:     resp.Total,
		Truncated: resp.Truncated,
	}
}

// attachPendingSignatures fills snap.PendingSignatures from the cached set.
// It leaves the field unset before the first successful poll.
func (a *Agent) attachPendingSignatures(snap *mitev1.StateSnapshot) {
	// The gate is read before scriptApprovalMu is taken. The two locks are
	// never held together, so the order cannot invert. A stale set must not
	// ride a snapshot between the poller's ticks.
	if !a.tenantGateIsCell() {
		return
	}
	a.scriptApprovalMu.Lock()
	defer a.scriptApprovalMu.Unlock()
	if a.scriptApprovalSet == nil {
		return
	}
	snap.PendingSignatures = a.scriptApprovalSet
}
