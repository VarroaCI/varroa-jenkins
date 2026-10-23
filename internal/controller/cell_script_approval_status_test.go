package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// recordForTest runs the recorder on a reconciler with no store, so no pool
// approves any signature.
func recordForTest(cr *v1alpha1.Controller, snap *mitev1.StateSnapshot) {
	_ = (&Reconciler{}).recordPendingSignatures(context.Background(), cr, snap)
}

func pendingCell() *v1alpha1.Controller {
	return &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}}}
}

// TestRecordPendingSignatures_NilSetKeepsStatus proves that a snapshot with
// no pending set leaves the value the status already holds.
func TestRecordPendingSignatures_NilSetKeepsStatus(t *testing.T) {
	cr := pendingCell()
	cr.Status.Cell = &v1alpha1.CellStatus{
		PendingSignatures:      []v1alpha1.CellPendingSignature{{Signature: "kept"}},
		PendingSignaturesTotal: 1,
	}
	recordForTest(cr, &mitev1.StateSnapshot{})
	if len(cr.Status.Cell.PendingSignatures) != 1 || cr.Status.Cell.PendingSignatures[0].Signature != "kept" {
		t.Fatalf("status changed on a nil set: %+v", cr.Status.Cell.PendingSignatures)
	}
	recordForTest(cr, nil)
	if cr.Status.Cell.PendingSignaturesTotal != 1 {
		t.Fatalf("total changed on a nil snapshot: %d", cr.Status.Cell.PendingSignaturesTotal)
	}
}

// TestRecordPendingSignatures_CapsItemsAndRequesters proves the bounds: at
// most 25 signatures, three requesters each, and no signature over 1024
// characters.
func TestRecordPendingSignatures_CapsItemsAndRequesters(t *testing.T) {
	var items []*mitev1.PendingSignature
	for i := 0; i < 30; i++ {
		var reqs []*mitev1.PendingSignatureRequester
		for j := 0; j < 5; j++ {
			reqs = append(reqs, &mitev1.PendingSignatureRequester{
				Tenant:      "tenant",
				JobPath:     fmt.Sprintf("team/job-%d-%d", i, j),
				BuildNumber: int64(j),
				RunId:       "run",
			})
		}
		items = append(items, &mitev1.PendingSignature{
			Signature:  fmt.Sprintf("sig.%d", i),
			Requesters: reqs,
		})
	}
	long := make([]byte, 1025)
	for i := range long {
		long[i] = 'a'
	}
	items = append(items, &mitev1.PendingSignature{Signature: string(long)})

	cr := pendingCell()
	recordForTest(cr, &mitev1.StateSnapshot{
		PendingSignatures: &mitev1.PendingSignatureSet{Items: items, Total: 31},
	})
	got := cr.Status.Cell.PendingSignatures
	if len(got) != maxPendingSignatures {
		t.Fatalf("len = %d, want %d", len(got), maxPendingSignatures)
	}
	for _, s := range got {
		if len(s.Requesters) != maxSignatureRequesters {
			t.Fatalf("requesters = %d, want %d", len(s.Requesters), maxSignatureRequesters)
		}
	}
	if cr.Status.Cell.PendingSignaturesTotal != 31 {
		t.Fatalf("total = %d, want 31", cr.Status.Cell.PendingSignaturesTotal)
	}
}

// TestRecordPendingSignatures_NonCellNoop proves that a controller with no
// cell role never receives a pending set.
func TestRecordPendingSignatures_NonCellNoop(t *testing.T) {
	cr := &v1alpha1.Controller{}
	recordForTest(cr, &mitev1.StateSnapshot{
		PendingSignatures: &mitev1.PendingSignatureSet{Items: []*mitev1.PendingSignature{{Signature: "x"}}, Total: 1},
	})
	if cr.Status.Cell != nil {
		t.Fatalf("a non-cell gained a cell status: %+v", cr.Status.Cell)
	}
}

// TestRecordPendingSignatures_DropsPoolApproved proves a signature that the
// cell pool approves leaves the pending list, and the total follows.
func TestRecordPendingSignatures_DropsPoolApproved(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:         "lts-2-555",
			ApprovedSignatures: []string{"method approved a"},
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}

	snap := &mitev1.StateSnapshot{PendingSignatures: &mitev1.PendingSignatureSet{
		Items: []*mitev1.PendingSignature{
			{Signature: "method approved a"},
			{Signature: "method open b"},
		},
		Total: 2,
	}}
	if err := rec.recordPendingSignatures(context.Background(), cr, snap); err != nil {
		t.Fatalf("recordPendingSignatures: %v", err)
	}
	got := cr.Status.Cell.PendingSignatures
	if len(got) != 1 || got[0].Signature != "method open b" {
		t.Fatalf("pending = %+v, want only the unapproved signature", got)
	}
	if cr.Status.Cell.PendingSignaturesTotal != 1 {
		t.Fatalf("total = %d, want 1", cr.Status.Cell.PendingSignaturesTotal)
	}
}
