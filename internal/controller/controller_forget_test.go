package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestReconcileController_DeletionForgetsTheMiteTransport pins that the
// deletion branch makes the transport drop the inbound subscription and the
// buffered results of the deleted controller.
func TestReconcileController_DeletionForgetsTheMiteTransport(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	tr := &captureTransport{}
	rec.miteTransport = tr

	now := metav1.Now()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting-cr",
			Namespace:         "ns",
			UID:               testUID,
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
	}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(tr.forgotten) != 1 || tr.forgotten[0] != "ns/deleting-cr" {
		t.Fatalf("forgotten = %v, want [ns/deleting-cr]", tr.forgotten)
	}
}

// TestReconcile_NotFoundForgetsTheMiteTransport pins that a controller with
// no object left is forgotten too, for a finalizer that was removed without
// the deletion branch.
func TestReconcile_NotFoundForgetsTheMiteTransport(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	tr := &captureTransport{}
	rec.miteTransport = tr

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "gone"}}
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(tr.forgotten) != 1 || tr.forgotten[0] != "ns/gone" {
		t.Fatalf("forgotten = %v, want [ns/gone]", tr.forgotten)
	}
}

// TestApproveRestart_RefusesAControllerUnderDeletion pins that an approve
// action does not send to a controller under deletion. The send would
// subscribe to the mite again after the deletion released it.
func TestApproveRestart_RefusesAControllerUnderDeletion(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	tr := &captureTransport{}
	rec.miteTransport = tr

	now := metav1.Now()
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting-cr",
			Namespace:         "ns",
			UID:               testUID,
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
	})

	err := rec.ApproveRestart(context.Background(), "", "ns", "deleting-cr", "approve")
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("err = %v, want a refusal for a controller under deletion", err)
	}
	if tr.lastDesired != nil {
		t.Fatal("ApproveRestart sent a desired state to a controller under deletion")
	}
}

// deletingSendTransport deletes the Controller while a send is in flight,
// the way a deletion that completes during ApproveRestart does.
type deletingSendTransport struct {
	*captureTransport
	onSend func()
}

func (d *deletingSendTransport) Send(ctx context.Context, ns, name string, msg *mitev1.OperatorMessage) error {
	d.onSend()
	return d.captureTransport.Send(ctx, ns, name, msg)
}

// TestApproveRestart_ForgetsAMiteDeletedDuringTheSend pins that an approve
// action releases the subscription its send made again when the deletion
// completed between the deletion check and the send.
func TestApproveRestart_ForgetsAMiteDeletedDuringTheSend(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("racing", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{}
	crdstore.MustSeed(client.store, cr)
	tr := &deletingSendTransport{captureTransport: &captureTransport{}}
	tr.onSend = func() {
		if err := crdstore.Delete[v1alpha1.Controller](context.Background(), client.store, "racing", "ns"); err != nil {
			t.Errorf("delete controller: %v", err)
		}
	}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "racing", "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if tr.lastDesired == nil {
		t.Fatal("ApproveRestart sent no desired state")
	}
	if len(tr.forgotten) != 1 || tr.forgotten[0] != "ns/racing" {
		t.Fatalf("forgotten = %v, want [ns/racing]", tr.forgotten)
	}
}

// TestApproveRestart_KeepsTheMiteOfALiveController pins that the second
// read forgets nothing for a controller that still exists.
func TestApproveRestart_KeepsTheMiteOfALiveController(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("live", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{}
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "live", "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(tr.forgotten) != 0 {
		t.Fatalf("forgotten = %v, want none", tr.forgotten)
	}
}

// TestForgetUnowned_ForgetsOnlyMitesOfUnheldShards pins that a replica
// forgets the mite of each controller in a shard that it does not hold, and
// keeps the mite of each controller that it owns.
func TestForgetUnowned_ForgetsOnlyMitesOfUnheldShards(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	ring := sharding.NewRing(256)
	held := sharding.NewShardSet()
	rec.SetShardOwnership(ring, held)

	var owned, moved string
	for i := 0; owned == "" || moved == ""; i++ {
		name := fmt.Sprintf("c%d", i)
		switch shard := ring.ShardFor("ns/" + name); {
		case owned == "":
			owned = name
			held.Add(shard)
		case !held.Held(shard):
			moved = name
		}
	}
	tr := &captureTransport{known: []string{"ns/" + owned, "ns/" + moved}}
	rec.miteTransport = tr

	rec.ForgetUnowned()

	if len(tr.forgotten) != 1 || tr.forgotten[0] != "ns/"+moved {
		t.Fatalf("forgotten = %v, want [ns/%s]", tr.forgotten, moved)
	}
}

// TestForgetUnowned_UnshardedForgetsNothing pins that a reconciler without
// shard ownership owns every controller and forgets no mite.
func TestForgetUnowned_UnshardedForgetsNothing(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	tr := &captureTransport{known: []string{"ns/a", "ns/b"}}
	rec.miteTransport = tr

	rec.ForgetUnowned()

	if len(tr.forgotten) != 0 {
		t.Fatalf("forgotten = %v, want none", tr.forgotten)
	}
}
