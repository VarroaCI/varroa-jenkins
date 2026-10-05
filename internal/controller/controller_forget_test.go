package controller

import (
	"context"
	"errors"
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

	err := rec.ApproveRestart(context.Background(), "", "ns", "deleting-cr", "approve", "")
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
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	tr := &deletingSendTransport{captureTransport: &captureTransport{}}
	tr.onSend = func() {
		if err := crdstore.Delete[v1alpha1.Controller](context.Background(), client.store, "racing", "ns"); err != nil {
			t.Errorf("delete controller: %v", err)
		}
	}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "racing", "approve", ""); err != nil {
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
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "live", "approve", ""); err != nil {
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

// TestApproveRestart_RefusesANonOwner pins that a replica that does not own
// the controller refuses the approve before any read, send, or status write.
func TestApproveRestart_RefusesANonOwner(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("elsewhere", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	rec.SetShardOwnership(sharding.NewRing(256), sharding.NewShardSet())
	tr := &captureTransport{}
	rec.miteTransport = tr

	err := rec.ApproveRestart(context.Background(), "", "ns", "elsewhere", "approve", "")
	if !errors.Is(err, ErrNotShardOwner) {
		t.Fatalf("err = %v, want ErrNotShardOwner", err)
	}
	if tr.lastDesired != nil {
		t.Fatal("a non-owner sent a desired state")
	}
	got, getErr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "elsewhere", "ns")
	if getErr != nil {
		t.Fatalf("get controller: %v", getErr)
	}
	if got.Status.PendingRestart == nil {
		t.Fatal("a non-owner cleared the pending restart")
	}
}

// TestApproveRestart_OwnerSends pins that the replica that holds the
// controller's shard sends the desired state.
func TestApproveRestart_OwnerSends(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	ring := sharding.NewRing(256)
	held := sharding.NewShardSet()
	held.Add(ring.ShardFor("ns/mine"))
	rec.SetShardOwnership(ring, held)
	tr := &captureTransport{}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if tr.lastDesired == nil {
		t.Fatal("the owner sent no desired state")
	}
}

// shardMovingTransport drops the controller's shard from the held set when
// a send arrives, the way a rebalance that runs during the send does.
type shardMovingTransport struct {
	*captureTransport
	onSend func()
}

func (s *shardMovingTransport) Send(ctx context.Context, ns, name string, msg *mitev1.OperatorMessage) error {
	s.onSend()
	return s.captureTransport.Send(ctx, ns, name, msg)
}

// TestApproveRestart_ForgetsTheMiteWhenTheShardMovesDuringTheSend pins that
// a replica that loses the shard during the send drops the subscription
// that the send made.
func TestApproveRestart_ForgetsTheMiteWhenTheShardMovesDuringTheSend(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("moving", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	ring := sharding.NewRing(256)
	held := sharding.NewShardSet()
	shard := ring.ShardFor("ns/moving")
	held.Add(shard)
	rec.SetShardOwnership(ring, held)
	tr := &shardMovingTransport{captureTransport: &captureTransport{}, onSend: func() { held.Remove(shard) }}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "moving", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(tr.forgotten) != 1 || tr.forgotten[0] != "ns/moving" {
		t.Fatalf("forgotten = %v, want [ns/moving]", tr.forgotten)
	}
}

// TestApproveDeletion_RefusesANonOwner pins that a replica that does not own
// the controller refuses the approve before any send or status write.
func TestApproveDeletion_RefusesANonOwner(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("elsewhere", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)
	rec.SetShardOwnership(sharding.NewRing(256), sharding.NewShardSet())
	tr := &captureTransport{}
	rec.miteTransport = tr

	err := rec.ApproveDeletion(context.Background(), "", "ns", "elsewhere", "team/job")
	if !errors.Is(err, ErrNotShardOwner) {
		t.Fatalf("err = %v, want ErrNotShardOwner", err)
	}
	if len(tr.imperatives) != 0 {
		t.Fatalf("a non-owner sent %d imperative commands", len(tr.imperatives))
	}
	if client.patchCount != 0 {
		t.Fatalf("a non-owner wrote the status %d times", client.patchCount)
	}
}

// TestApproveDeletion_OwnerSendsAndClears pins that the replica that holds
// the controller's shard sends the delete and clears the pending entry.
func TestApproveDeletion_OwnerSendsAndClears(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)
	ring := sharding.NewRing(256)
	held := sharding.NewShardSet()
	held.Add(ring.ShardFor("ns/mine"))
	rec.SetShardOwnership(ring, held)
	tr := &captureTransport{}
	rec.miteTransport = tr

	if err := rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job"); err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	if len(tr.imperatives) != 1 {
		t.Fatalf("imperative commands = %d, want 1", len(tr.imperatives))
	}
	if got := tr.imperatives[0]; got.Type != mitev1.CommandTypeDeleteItem || got.Target != "team/job" {
		t.Fatalf("command = %v %q, want %v %q", got.Type, got.Target, mitev1.CommandTypeDeleteItem, "team/job")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	st := &got.Status
	if len(st.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %v, want none", st.PendingItemDeletions)
	}
	var found bool
	for _, c := range st.Conditions {
		if c.Type == v1alpha1.ConditionDeletionPending {
			found = true
			if c.Status != metav1.ConditionFalse {
				t.Fatalf("DeletionPending = %s, want False", c.Status)
			}
		}
	}
	if !found {
		t.Fatal("DeletionPending condition missing")
	}
}

// shardMovingImperativeTransport runs onSend inside SendImperative, so a
// test can move the shard while the delete is in flight.
type shardMovingImperativeTransport struct {
	*captureTransport
	onSend func()
}

func (s *shardMovingImperativeTransport) SendImperative(ctx context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	s.onSend()
	return s.captureTransport.SendImperative(ctx, ns, name, cmd)
}

// TestApproveDeletion_SkipsTheStatusWriteWhenTheShardMovesDuringTheSend
// pins that a replica that loses the shard during the send does not write
// the status from its stale read.
func TestApproveDeletion_SkipsTheStatusWriteWhenTheShardMovesDuringTheSend(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("moving", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)
	ring := sharding.NewRing(256)
	held := sharding.NewShardSet()
	shard := ring.ShardFor("ns/moving")
	held.Add(shard)
	rec.SetShardOwnership(ring, held)
	tr := &shardMovingImperativeTransport{captureTransport: &captureTransport{}, onSend: func() { held.Remove(shard) }}
	rec.miteTransport = tr

	if err := rec.ApproveDeletion(context.Background(), "", "ns", "moving", "team/job"); err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	if len(tr.imperatives) != 1 {
		t.Fatalf("imperative commands = %d, want 1", len(tr.imperatives))
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "moving", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if len(got.Status.PendingItemDeletions) != 1 || got.ResourceVersion != "1" {
		t.Fatalf("a replica that lost the shard wrote the status: pending %v, resourceVersion %q",
			got.Status.PendingItemDeletions, got.ResourceVersion)
	}
}

// afterNthControllerGet runs fn once, on the nth Controller read after arm
// is called. fn runs between that read and the caller's write, the window
// in which a concurrent writer moves the resourceVersion.
func afterNthControllerGet(t *testing.T, store *crdstore.Fake, n int, fn func()) (arm func(), fired func() bool) {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("controller gvr: %v", err)
	}
	armed, count, done := false, 0, false
	store.SetAfterGet(gvr, func(string, string) {
		if !armed || done {
			return
		}
		count++
		if count == n {
			done = true
			fn()
		}
	})
	return func() { armed = true }, func() bool { return done }
}

// ownerStatusWrite merge-patches the status of ns/mine the way the owner's
// reconcile does.
func ownerStatusWrite(t *testing.T, store *crdstore.Fake, status map[string]any) {
	t.Helper()
	if err := crdstore.PatchStatus[v1alpha1.Controller](context.Background(), store, "mine", "ns", status); err != nil {
		t.Errorf("owner status write: %v", err)
	}
}

// TestApproveDeletion_KeepsAnOwnerWriteThatLandsBeforeTheStatusWrite pins
// that an owner write between the approve's read and its status write
// survives, and that the approve removes only its own entry.
func TestApproveDeletion_KeepsAnOwnerWriteThatLandsBeforeTheStatusWrite(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)
	arm, fired := afterNthControllerGet(t, client.store, 1, func() {
		ownerStatusWrite(t, client.store, map[string]any{
			"configHash":           "owner",
			"pendingItemDeletions": []any{map[string]any{"path": "team/job"}, map[string]any{"path": "team/other"}},
		})
	})
	rec.miteTransport = &shardMovingImperativeTransport{captureTransport: &captureTransport{}, onSend: arm}

	if err := rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job"); err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	if !fired() {
		t.Fatal("the owner write did not run")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if got.Status.ConfigHash != "owner" {
		t.Fatalf("configHash = %q, want the owner's value", got.Status.ConfigHash)
	}
	if len(got.Status.PendingItemDeletions) != 1 || got.Status.PendingItemDeletions[0].Path != "team/other" {
		t.Fatalf("pending deletions = %v, want [team/other]", got.Status.PendingItemDeletions)
	}
}

// TestApproveDeletion_EntryAlreadyClearedWritesNothing pins that an approve
// whose entry the owner already cleared makes no status write.
func TestApproveDeletion_EntryAlreadyClearedWritesNothing(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)
	tr := &shardMovingImperativeTransport{captureTransport: &captureTransport{}, onSend: func() {
		ownerStatusWrite(t, client.store, map[string]any{"pendingItemDeletions": []any{}})
	}}
	rec.miteTransport = tr

	if err := rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job"); err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if got.ResourceVersion != "2" {
		t.Fatalf("resourceVersion = %q, want 2 (only the owner write)", got.ResourceVersion)
	}
}

// TestApproveRestart_PluginRollKeepsAConcurrentOwnerWrite pins that a
// plugin-roll approval keeps an owner write between its read and its write.
func TestApproveRestart_PluginRollKeepsAConcurrentOwnerWrite(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)
	arm, fired := afterNthControllerGet(t, client.store, 2, func() {
		ownerStatusWrite(t, client.store, map[string]any{"configHash": "owner"})
	})
	arm()
	rec.miteTransport = &captureTransport{}

	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", ""); err != nil {
		t.Fatalf("approve plugin roll: %v", err)
	}
	if !fired() {
		t.Fatal("the owner write did not run")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if got.Status.ConfigHash != "owner" {
		t.Fatalf("configHash = %q, want the owner's value", got.Status.ConfigHash)
	}
	if got.Status.ApprovedPluginRollChecksum != "abc" || got.Status.PendingPluginRoll != nil {
		t.Fatalf("approved = %q pending = %v, want abc and none",
			got.Status.ApprovedPluginRollChecksum, got.Status.PendingPluginRoll)
	}
}

// TestApproveRestart_PluginRollRefusesAChangedTarget pins that an approval
// does not approve a pending target that changed after the approve read it.
func TestApproveRestart_PluginRollRefusesAChangedTarget(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)
	arm, _ := afterNthControllerGet(t, client.store, 2, func() {
		ownerStatusWrite(t, client.store, map[string]any{"pendingPluginRoll": map[string]any{"targetChecksum": "def"}})
	})
	arm()
	rec.miteTransport = &captureTransport{}

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", "")
	if !errors.Is(err, ErrPluginRollChanged) {
		t.Fatalf("err = %v, want ErrPluginRollChanged", err)
	}
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("get controller: %v", gerr)
	}
	if got.Status.ApprovedPluginRollChecksum != "" {
		t.Fatalf("approved = %q, want none", got.Status.ApprovedPluginRollChecksum)
	}
	if got.Status.PendingPluginRoll == nil || got.Status.PendingPluginRoll.TargetChecksum != "def" {
		t.Fatalf("pending = %v, want target def", got.Status.PendingPluginRoll)
	}
}

// TestApproveRestart_RestartTakesTheAttemptCountFromTheFreshRead pins that
// a restart approval builds the drain state on a fresh read, so an owner
// write during the send survives.
func TestApproveRestart_RestartTakesTheAttemptCountFromTheFreshRead(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	cr.Status.RestartDrain = &v1alpha1.RestartDrainStatus{AttemptCount: 1}
	crdstore.MustSeed(client.store, cr)
	// The second read after the send is the compare-and-swap read: the
	// first is the deletion check.
	arm, fired := afterNthControllerGet(t, client.store, 2, func() {
		ownerStatusWrite(t, client.store, map[string]any{
			"configHash":   "owner",
			"restartDrain": map[string]any{"attemptCount": 3},
		})
	})
	tr := &deletingSendTransport{captureTransport: &captureTransport{}, onSend: arm}
	rec.miteTransport = tr

	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "restart", ""); err != nil {
		t.Fatalf("approve restart: %v", err)
	}
	if !fired() {
		t.Fatal("the owner write did not run")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if got.Status.ConfigHash != "owner" {
		t.Fatalf("configHash = %q, want the owner's value", got.Status.ConfigHash)
	}
	if got.Status.PendingRestart != nil {
		t.Fatalf("pending restart = %v, want none", got.Status.PendingRestart)
	}
	d := got.Status.RestartDrain
	if d == nil || d.AttemptCount != 3 || d.CommandID == "" {
		t.Fatalf("restart drain = %+v, want attempt 3 with a command ID", d)
	}
	if d.DesiredStateHash == "" || d.DesiredStateHash != got.Status.DesiredStateHash {
		t.Fatalf("drain hash = %q, status hash = %q, want the same non-empty hash", d.DesiredStateHash, got.Status.DesiredStateHash)
	}
}

// TestApproveRestart_KeepsAPendingRestartRaisedAfterTheRead pins that an
// approval clears only the pending restart that it read. A newer pending
// restart stays for the user to approve.
func TestApproveRestart_KeepsAPendingRestartRaisedAfterTheRead(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	cr.Status.PendingRestart.DesiredStateHash = "a"
	crdstore.MustSeed(client.store, cr)
	arm, fired := afterNthControllerGet(t, client.store, 2, func() {
		ownerStatusWrite(t, client.store, map[string]any{"pendingRestart": map[string]any{"desiredStateHash": "b"}})
	})
	rec.miteTransport = &deletingSendTransport{captureTransport: &captureTransport{}, onSend: arm}

	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !fired() {
		t.Fatal("the owner write did not run")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if got.Status.PendingRestart == nil || got.Status.PendingRestart.DesiredStateHash != "b" {
		t.Fatalf("pending restart = %+v, want the newer pending restart b", got.Status.PendingRestart)
	}
	if got.Status.DesiredStateHash == "" {
		t.Fatal("desiredStateHash is empty, want the hash of the push")
	}
}

// TestApprovePluginRoll_NoPendingRollRefuses pins that an approval that
// finds no pending roll at its write approves nothing and says so.
func TestApprovePluginRoll_NoPendingRollRefuses(t *testing.T) {
	st := &v1alpha1.ControllerStatus{}
	changed, err := approvePluginRoll(st, "abc")
	if !errors.Is(err, ErrNoPendingPluginRoll) || changed {
		t.Fatalf("approvePluginRoll = %v, %v; want false, ErrNoPendingPluginRoll", changed, err)
	}
	if st.ApprovedPluginRollChecksum != "" {
		t.Fatalf("approved = %q, want none", st.ApprovedPluginRollChecksum)
	}
}
