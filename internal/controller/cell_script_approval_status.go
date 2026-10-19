package controller

import (
	"context"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// maxRefusalReasonLength bounds the signature text in a refusal event.
const maxRefusalReasonLength = 256

// refusalKey identifies one refused signature of one build of one job.
type refusalKey struct {
	signature   string
	jobPath     string
	buildNumber int64
}

const (
	// maxPendingSignatures bounds status.cell.pendingSignatures. The CRD
	// carries the same MaxItems.
	maxPendingSignatures = 25
	// maxSignatureRequesters bounds the requesters of one signature.
	maxSignatureRequesters = 3
	// maxSignatureLength bounds one signature string. A longer value is
	// dropped, because the CRD caps the field at the same length.
	maxSignatureLength = 1024
)

// recordPendingSignatures copies the refused script signatures the mite
// reported into cr.Status.Cell. It drops every signature the cell pool
// already approves, because the plugin reports refusals and does not read
// the approval list. It writes nothing when cr is not a cell or when the
// snapshot carries no pending set, so the status keeps its last value. An
// empty result clears the status.
//
// A signature that the cell bundle approves, and the pool does not, stays
// listed. The pool declaration is the operator's approval surface.
func (r *Reconciler) recordPendingSignatures(ctx context.Context, cr *v1alpha1.Controller, snap *mitev1.StateSnapshot) error {
	if !v1alpha1.IsCell(cr) || snap == nil || snap.PendingSignatures == nil {
		return nil
	}
	approved, err := r.poolSignatures(ctx, cr)
	if err != nil {
		return err
	}
	approvedSet := make(map[string]bool, len(approved))
	for _, s := range approved {
		approvedSet[s] = true
	}
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	seen := refusalKeys(cr.Status.Cell.PendingSignatures)
	set := snap.PendingSignatures
	out := make([]v1alpha1.CellPendingSignature, 0, min(len(set.Items), maxPendingSignatures))
	removed := 0
	for _, item := range set.Items {
		if item == nil || item.Signature == "" || len(item.Signature) > maxSignatureLength {
			continue
		}
		if approvedSet[item.Signature] {
			removed++
			continue
		}
		if len(out) == maxPendingSignatures {
			continue
		}
		out = append(out, v1alpha1.CellPendingSignature{
			Signature:  item.Signature,
			Dangerous:  item.Dangerous,
			Requesters: convertRequesters(item.Requesters),
		})
	}
	r.publishNewRefusals(cr, out, seen)
	cr.Status.Cell.PendingSignatures = out
	total := set.Total - removed
	if total < len(out) {
		total = len(out)
	}
	cr.Status.Cell.PendingSignaturesTotal = int32(total)
	return nil
}

// convertRequesters copies at most maxSignatureRequesters requesters.
func convertRequesters(in []*mitev1.PendingSignatureRequester) []v1alpha1.CellSignatureRequester {
	if len(in) == 0 {
		return nil
	}
	out := make([]v1alpha1.CellSignatureRequester, 0, min(len(in), maxSignatureRequesters))
	for _, r := range in {
		if len(out) == maxSignatureRequesters {
			break
		}
		if r == nil {
			continue
		}
		out = append(out, v1alpha1.CellSignatureRequester{
			Tenant:      r.Tenant,
			JobPath:     r.JobPath,
			BuildNumber: r.BuildNumber,
			RunID:       r.RunId,
		})
	}
	return out
}

// refusalKeys returns the signature, job, and build triples of a status list.
func refusalKeys(list []v1alpha1.CellPendingSignature) map[refusalKey]bool {
	keys := map[refusalKey]bool{}
	for _, p := range list {
		for _, req := range p.Requesters {
			keys[refusalKey{signature: p.Signature, jobPath: req.JobPath, buildNumber: req.BuildNumber}] = true
		}
	}
	return keys
}

// publishNewRefusals publishes one cell.signature_refused event for each
// refusal that refusalEvents reports. A nil publisher publishes nothing.
func (r *Reconciler) publishNewRefusals(cr *v1alpha1.Controller, now []v1alpha1.CellPendingSignature, seen map[refusalKey]bool) {
	if r.activityPublisher == nil {
		return
	}
	for _, e := range refusalEvents(cr, now, seen) {
		r.activityPublisher.Publish(e)
	}
}

// refusalEvents returns one cell.signature_refused event for each tenant
// requester whose refusal the previous status (seen) did not hold. It adds
// each new key to seen, so a repeat status returns no event.
func refusalEvents(cr *v1alpha1.Controller, now []v1alpha1.CellPendingSignature, seen map[refusalKey]bool) []activity.Event {
	var out []activity.Event
	for _, p := range now {
		for _, req := range p.Requesters {
			if req.Tenant == "" {
				continue
			}
			key := refusalKey{signature: p.Signature, jobPath: req.JobPath, buildNumber: req.BuildNumber}
			if seen[key] {
				continue
			}
			seen[key] = true
			reason := p.Signature
			if len(reason) > maxRefusalReasonLength {
				reason = reason[:maxRefusalReasonLength]
			}
			out = append(out, activity.Event{
				Type:        "cell.signature_refused",
				Source:      "operator",
				Severity:    "warning",
				Namespace:   cr.Namespace,
				Controller:  cr.Name,
				ItemPath:    req.JobPath,
				BuildNumber: req.BuildNumber,
				Reason:      reason,
				Message:     req.JobPath + " used a refused script signature in cell " + cr.Name,
			})
		}
	}
	return out
}
