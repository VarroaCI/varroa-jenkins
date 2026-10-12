package controller

import (
	"context"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

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
