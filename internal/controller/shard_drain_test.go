package controller

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// drainResult is what a drain that starts during shard-owned work reports.
type drainResult struct {
	busy []int
	// patches is the PatchControllerStatus count when the drain ended.
	patches int
	// approved is the stored approved plugin-roll checksum when the drain
	// ended.
	approved string
}

// drainDuringWork installs a hook that starts a drain of shard right after
// the first read of ns/mine. The hook then holds the work for hold, and
// fails the test when the drain ends inside that time. The returned channel
// receives the drain result.
func drainDuringWork(t *testing.T, client *testClient, set *sharding.ShardSet, shard int,
	wait, cancelWait, hold time.Duration) <-chan drainResult {
	t.Helper()
	gvr := mustControllerGVR(t)
	var fired atomic.Bool
	result := make(chan drainResult, 1)
	client.store.SetAfterGet(gvr, func(string, string) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		ended := make(chan struct{})
		go func() {
			busy := set.Drain([]int{shard}, wait, cancelWait)
			res := drainResult{busy: busy, patches: client.patchCount}
			if got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns"); err == nil {
				res.approved = got.Status.ApprovedPluginRollChecksum
			}
			close(ended)
			result <- res
		}()
		select {
		case <-ended:
			t.Error("the drain ended while the work ran")
		case <-time.After(hold):
		}
	})
	return result
}

func receiveDrain(t *testing.T, result <-chan drainResult) drainResult {
	t.Helper()
	select {
	case res := <-result:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not end")
		return drainResult{}
	}
}

// shardedReplica builds a reconciler on client that uses set as its held
// shards. Two such reconcilers on one client act as two operator replicas
// on one apiserver.
func shardedReplica(client *testClient, ring *sharding.Ring, set *sharding.ShardSet) *Reconciler {
	rec := newTestReconciler(client)
	rec.miteTransport = &captureTransport{}
	rec.SetShardOwnership(ring, set)
	return rec
}

var mineRequest = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "mine"}}

// TestReconcile_ShardReleaseWaitsForTheTickEndWrite moves the shard during a
// tick. The release waits until the old owner's end write lands, and only
// then does the new owner get the shard. So the old owner's end write never
// lands after the new owner's write.
func TestReconcile_ShardReleaseWaitsForTheTickEndWrite(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	oldOwner := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	crdstore.MustSeed(client.store, cr)

	result := drainDuringWork(t, client, setA, shard, 5*time.Second, time.Second, 100*time.Millisecond)
	if _, err := oldOwner.Reconcile(context.Background(), mineRequest); err != nil {
		t.Fatalf("old owner reconcile: %v", err)
	}
	oldWrites := client.patchCount
	if oldWrites == 0 {
		t.Fatal("the old owner wrote no status")
	}
	res := receiveDrain(t, result)
	if len(res.busy) != 0 {
		t.Fatalf("busy shards = %v, want none", res.busy)
	}
	if res.patches != oldWrites {
		t.Fatalf("status writes when the drain ended = %d, want all %d of the old owner", res.patches, oldWrites)
	}

	// The shard manager claims the shard for the new owner only after the
	// drain released the lease.
	client.store.SetAfterGet(mustControllerGVR(t), nil)
	setB.Add(shard)
	if _, err := oldOwner.Reconcile(context.Background(), mineRequest); err != nil {
		t.Fatalf("old owner reconcile after the move: %v", err)
	}
	if client.patchCount != oldWrites {
		t.Fatal("the old owner wrote status after the release")
	}
	if _, err := newOwner.Reconcile(context.Background(), mineRequest); err != nil {
		t.Fatalf("new owner reconcile: %v", err)
	}
	if client.patchCount <= oldWrites {
		t.Fatal("the new owner wrote no status")
	}
}

// TestReconcile_ShardReleaseCancelsALongTick pins that a tick that runs past
// the drain wait is cancelled and does not send its end write.
func TestReconcile_ShardReleaseCancelsALongTick(t *testing.T) {
	client := newTestClient()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	set := sharding.NewShardSet()
	set.Add(shard)
	rec := shardedReplica(client, ring, set)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	crdstore.MustSeed(client.store, cr)

	result := drainDuringWork(t, client, set, shard, 10*time.Millisecond, 2*time.Second, 200*time.Millisecond)
	if _, err := rec.Reconcile(context.Background(), mineRequest); err == nil {
		t.Fatal("a cancelled tick returned no error")
	}
	if client.patchCount != 0 {
		t.Fatalf("status writes = %d, want none from a cancelled tick", client.patchCount)
	}
	if res := receiveDrain(t, result); len(res.busy) != 0 {
		t.Fatalf("busy shards = %v, want none", res.busy)
	}
}

// TestApproveRestart_ShardReleaseWaitsForTheApprove pins that a release
// waits for an approve in flight, so the approve's write lands before
// another replica can own the controller.
func TestApproveRestart_ShardReleaseWaitsForTheApprove(t *testing.T) {
	client := newTestClient()
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	set := sharding.NewShardSet()
	set.Add(shard)
	rec := shardedReplica(client, ring, set)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)

	result := drainDuringWork(t, client, set, shard, 5*time.Second, time.Second, 100*time.Millisecond)
	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", ""); err != nil {
		t.Fatalf("approve plugin roll: %v", err)
	}
	res := receiveDrain(t, result)
	if len(res.busy) != 0 {
		t.Fatalf("busy shards = %v, want none", res.busy)
	}
	if res.approved != "abc" {
		t.Fatalf("approved checksum when the drain ended = %q, want abc", res.approved)
	}
}

func mustControllerGVR(t *testing.T) schema.GroupVersionResource {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("controller gvr: %v", err)
	}
	return gvr
}

// TestApproveDeletion_SendsNothingAfterARevoke pins that an approve whose
// shard is revoked after its read sends no delete to the mite.
func TestApproveDeletion_SendsNothingAfterARevoke(t *testing.T) {
	client := newTestClient()
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	set := sharding.NewShardSet()
	set.Add(shard)
	rec := shardedReplica(client, ring, set)
	tr := &captureTransport{}
	rec.miteTransport = tr
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)

	var fired atomic.Bool
	client.store.SetAfterGet(mustControllerGVR(t), func(string, string) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		set.Revoke(shard)
	})
	if err := rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job"); err == nil {
		t.Fatal("a revoked approve returned no error")
	}
	if len(tr.imperatives) != 0 {
		t.Fatalf("imperatives = %v, want none after a revoke", tr.imperatives)
	}
}

// stallAfterFirstRead installs a hook that runs during the first read of
// ns/mine. The read already took its copy, so the hook models a replica that
// stalls right after its tick read. During the stall, newOwner runs.
func stallAfterFirstRead(t *testing.T, client *testClient, newOwner func()) {
	t.Helper()
	var fired atomic.Bool
	client.store.SetAfterGet(mustControllerGVR(t), func(_, name string) {
		if name != "mine" || !fired.CompareAndSwap(false, true) {
			return
		}
		newOwner()
	})
}

// TestReconcile_StalledReplicaEndWriteDoesNotLandAfterNewOwner models a
// replica that stalls past its shard lease in the middle of a tick. Its held
// set still names the shard, because no rebalance pass ran during the stall.
// The new owner writes during the stall. The stalled tick then resumes, and
// its end write conflicts instead of landing over the new owner's write.
func TestReconcile_StalledReplicaEndWriteDoesNotLandAfterNewOwner(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	crdstore.MustSeed(client.store, cr)

	var ownerRV string
	var ownerStatus v1alpha1.ControllerStatus
	stallAfterFirstRead(t, client, func() {
		if _, err := newOwner.Reconcile(context.Background(), mineRequest); err != nil {
			t.Errorf("new owner reconcile: %v", err)
		}
		got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
		if err != nil {
			t.Errorf("read after the new owner: %v", err)
			return
		}
		ownerRV = got.ResourceVersion
		ownerStatus = got.Status
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("stalled replica reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("stalled replica RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	if ownerRV == "" || ownerRV == "1" {
		t.Fatalf("new owner resourceVersion = %q, want a write after 1", ownerRV)
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if got.ResourceVersion != ownerRV {
		t.Fatalf("resourceVersion = %q, want the new owner's %q: the stalled write landed", got.ResourceVersion, ownerRV)
	}
	if !reflect.DeepEqual(got.Status, ownerStatus) {
		t.Fatalf("status changed after the new owner's write:\n got %+v\nwant %+v", got.Status, ownerStatus)
	}
}

// TestReconcile_StalledReplicaCannotRevertNewOwnersApprove pins that a
// stalled tick cannot write back the approve fields it read before the new
// owner accepted an approve.
func TestReconcile_StalledReplicaCannotRevertNewOwnersApprove(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)

	stallAfterFirstRead(t, client, func() {
		if err := newOwner.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", ""); err != nil {
			t.Errorf("new owner approve: %v", err)
		}
	})

	if _, err := stalled.Reconcile(context.Background(), mineRequest); err != nil {
		t.Fatalf("stalled replica reconcile: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if got.Status.ApprovedPluginRollChecksum != "abc" {
		t.Fatalf("approved checksum = %q, want abc: the stalled tick reverted the approve", got.Status.ApprovedPluginRollChecksum)
	}
}

// idleConnectedController returns ns/mine as a Connected controller that
// passes every auto-hibernate gate under idleTransport. It carries its wake
// token, label, and finalizer, so the tick sends no write to it before the
// hibernation decision.
func idleConnectedController() *v1alpha1.Controller {
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Labels = map[string]string{v1alpha1.LabelControllerName: "mine"}
	cr.Finalizers = []string{finalizerName}
	cr.Spec.PowerState = "Running"
	cr.Spec.Hibernation = &v1alpha1.HibernationSpec{Enabled: true, GracePeriodMinutes: 60}
	connected := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	cr.Status.FirstConnectedAt = &connected
	cr.Status.WakeToken = "wake-token"
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "JenkinsHealthy",
		LastTransitionTime: connected,
	}}
	return cr
}

// TestReconcile_StalledReplicaDoesNotHibernateAfterNewOwnerWakes models a
// replica that reads an idle controller and then stalls past its shard
// lease. During the stall, the new owner hibernates the controller and then
// wakes it. The stalled tick resumes with its auto-hibernate decision from
// the old read. Its hibernation write must conflict, so the controller stays
// awake.
func TestReconcile_StalledReplicaDoesNotHibernateAfterNewOwnerWakes(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	stalled.miteTransport = &idleTransport{captureTransport: &captureTransport{}}
	newOwner := shardedReplica(client, ring, setB)
	crdstore.MustSeed(client.store, idleConnectedController())

	var ownerRV string
	stallAfterFirstRead(t, client, func() {
		ctx := context.Background()
		if err := newOwner.HibernateController(ctx, "ns", "mine"); err != nil {
			t.Errorf("new owner hibernate: %v", err)
		}
		if err := newOwner.WakeControllerAction(ctx, "ns", "mine"); err != nil {
			t.Errorf("new owner wake: %v", err)
		}
		got, err := crdstore.Get[v1alpha1.Controller](ctx, client.store, "mine", "ns")
		if err != nil {
			t.Errorf("read after the wake: %v", err)
			return
		}
		ownerRV = got.ResourceVersion
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("stalled replica reconcile: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if got.Status.Hibernated {
		t.Fatal("the stalled replica hibernated the controller again after the new owner woke it")
	}
	if got.ResourceVersion != ownerRV {
		t.Fatalf("resourceVersion = %q, want the new owner's %q: a stalled write landed", got.ResourceVersion, ownerRV)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("stalled replica RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
}

// TestReconcile_StalledReplicaStopClearDoesNotLandAfterNewerWrite models a
// replica that reads a Stopped, hibernated controller and then stalls. A
// newer write changes the Controller during the stall. The stalled tick's
// hibernation clear must conflict and stop the tick.
func TestReconcile_StalledReplicaStopClearDoesNotLandAfterNewerWrite(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	rec := newTestReconciler(client)
	cr := idleConnectedController()
	cr.Spec.PowerState = "Stopped"
	cr.Status.Phase = v1alpha1.ControllerPhaseHibernated
	cr.Status.Hibernated = true
	crdstore.MustSeed(client.store, cr)
	gvr := mustControllerGVR(t)

	var newerRV string
	stallAfterFirstRead(t, client, func() {
		if err := client.store.PatchObjectStatus(context.Background(), gvr, "ns", "mine", map[string]any{"configHash": "newer"}); err != nil {
			t.Errorf("newer write: %v", err)
		}
		got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
		if err != nil {
			t.Errorf("read after the newer write: %v", err)
			return
		}
		newerRV = got.ResourceVersion
	})

	res, err := rec.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if !got.Status.Hibernated || got.ResourceVersion != newerRV {
		t.Fatalf("hibernated = %v at resourceVersion %q, want true at %q: the stale clear landed",
			got.Status.Hibernated, got.ResourceVersion, newerRV)
	}
	if len(client.tickHibernatedWrites) != 1 || client.tickHibernatedWrites[0].resourceVersion != "1" {
		t.Fatalf("tick hibernation writes = %+v, want one at the tick's read version 1", client.tickHibernatedWrites)
	}
}

// stoppedStalledController returns ns/mine as a Stopped controller that
// carries its label and finalizer. The tick therefore sends no meta write
// before it reaches the StatefulSet scale.
func stoppedStalledController() *v1alpha1.Controller {
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseStopped)
	cr.ResourceVersion = "1"
	cr.Labels = map[string]string{v1alpha1.LabelControllerName: "mine"}
	cr.Finalizers = []string{finalizerName}
	cr.Spec.PowerState = "Stopped"
	return cr
}

// requireStalledScaleFenced checks the outcome of a stalled tick that resumed
// after the new owner brought the StatefulSet up. The tick must requeue
// without an error, the StatefulSet must keep its replica, and the tick must
// have reached the scale site.
func requireStalledScaleFenced(t *testing.T, client *testClient, res reconcile.Result, err error, prefix string) {
	t.Helper()
	if err != nil {
		t.Fatalf("stalled replica reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("stalled replica RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	if client.stsReplicas[prefix] != 1 {
		t.Fatalf("StatefulSet replicas = %d, want 1: the stalled tick scaled it down", client.stsReplicas[prefix])
	}
	if client.scaleFenceCalls < 1 {
		t.Fatalf("scale fence calls = %d, want at least 1: the stalled tick did not reach the scale site", client.scaleFenceCalls)
	}
}

// TestReconcile_StalledReplicaDoesNotScaleDownAfterPowerOn models a replica
// that reads a Stopped controller and stalls. The user then powers the
// controller on, and the new owner scales the StatefulSet up. The stalled
// scale-down must conflict instead of scaling the workload to 0.
func TestReconcile_StalledReplicaDoesNotScaleDownAfterPowerOn(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := stoppedStalledController()
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 0}

	stallAfterFirstRead(t, client, func() {
		ctx := context.Background()
		got, err := crdstore.Get[v1alpha1.Controller](ctx, client.store, "mine", "ns")
		if err != nil {
			t.Fatalf("read before power on: %v", err)
		}
		got.Spec.PowerState = "Running"
		if err := crdstore.Update(ctx, client.store, got); err != nil {
			t.Fatalf("power on: %v", err)
		}
		for i := 0; i < 5 && client.stsReplicas[prefix] != 1; i++ {
			if _, err := newOwner.Reconcile(ctx, mineRequest); err != nil {
				t.Fatalf("new owner reconcile: %v", err)
			}
		}
		if client.stsReplicas[prefix] != 1 {
			t.Fatalf("new owner did not scale the StatefulSet up: replicas = %d", client.stsReplicas[prefix])
		}
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	requireStalledScaleFenced(t, client, res, err, prefix)
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	if got.Spec.PowerState != "Running" {
		t.Fatalf("powerState = %q, want Running", got.Spec.PowerState)
	}
}

// TestReconcile_StalledReplicaDoesNotScaleDownAfterWake models a replica that
// reads a hibernated controller and stalls. The new owner wakes the
// controller and brings the StatefulSet up. The stalled hibernated tick must
// conflict instead of scaling the workload to 0.
func TestReconcile_StalledReplicaDoesNotScaleDownAfterWake(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := idleConnectedController()
	cr.Status.Phase = v1alpha1.ControllerPhaseHibernated
	cr.Status.Hibernated = true
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 0}

	stallAfterFirstRead(t, client, func() {
		ctx := context.Background()
		if err := newOwner.WakeControllerAction(ctx, "ns", "mine"); err != nil {
			t.Fatalf("new owner wake: %v", err)
		}
		if err := client.CreateStatefulSet(ctx, StatefulSetSpec{Name: prefix, Namespace: "ns", Replicas: ptr32(1)}, noStatefulSetFence); err != nil {
			t.Fatalf("new owner scale up: %v", err)
		}
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	requireStalledScaleFenced(t, client, res, err, prefix)
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	if got.Status.Hibernated {
		t.Fatal("status.hibernated = true, want false after the wake")
	}
}

// TestReconcile_ScaleDownConflictsWhenStatefulSetChangesAfterFence covers a
// StatefulSet that changes between the tick fence and the scale patch. The
// precondition must conflict. The tick then requeues and reports no block.
func TestReconcile_ScaleDownConflictsWhenStatefulSetChangesAfterFence(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	set := sharding.NewShardSet()
	set.Add(shard)
	rec := shardedReplica(client, ring, set)
	cr := stoppedStalledController()
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 1}
	client.afterScaleFence = func() {
		if err := client.CreateStatefulSet(context.Background(), StatefulSetSpec{Name: prefix, Namespace: "ns", Replicas: ptr32(1)}, noStatefulSetFence); err != nil {
			t.Errorf("concurrent statefulset write: %v", err)
		}
	}

	res, err := rec.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	if client.stsReplicas[prefix] != 1 {
		t.Fatalf("StatefulSet replicas = %d, want 1", client.stsReplicas[prefix])
	}
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == v1alpha1.ConditionReconcileBlocked && c.Status == metav1.ConditionTrue {
			t.Fatalf("ReconcileBlocked=True, want no block after a fenced conflict")
		}
	}
}

// provisioningStalledController returns ns/mine as a Running controller in
// Provisioning that carries its label and finalizer. The tick therefore sends
// no meta write before it reaches the StatefulSet create or update.
func provisioningStalledController() *v1alpha1.Controller {
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.ResourceVersion = "1"
	cr.Labels = map[string]string{v1alpha1.LabelControllerName: "mine"}
	cr.Finalizers = []string{finalizerName}
	return cr
}

// requireStalledScaleUpFenced checks the outcome of a stalled provisioning
// tick that resumed after the new owner powered the controller off. The tick
// must requeue without an error, the StatefulSet must keep 0 replicas, and the
// tick must have reached the scale-up fence.
func requireStalledScaleUpFenced(t *testing.T, client *testClient, res reconcile.Result, err error, prefix string) {
	t.Helper()
	if err != nil {
		t.Fatalf("stalled replica reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("stalled replica RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	if client.stsReplicas[prefix] != 0 {
		t.Fatalf("StatefulSet replicas = %d, want 0: the stalled tick scaled a powered-off controller up", client.stsReplicas[prefix])
	}
	if client.createFenceCalls < 1 {
		t.Fatalf("create fence calls = %d, want at least 1: the stalled tick did not reach the scale-up site", client.createFenceCalls)
	}
}

// TestReconcile_StalledReplicaDoesNotScaleUpAfterPowerOff models a replica
// that reads a Running controller in Provisioning and stalls. The user then
// powers the controller off, and the new owner scales the StatefulSet to 0.
// The stalled tick must conflict instead of writing replicas 1.
func TestReconcile_StalledReplicaDoesNotScaleUpAfterPowerOff(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := provisioningStalledController()
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 1}
	client.stsRV = map[string]int{prefix: 1}

	stallAfterFirstRead(t, client, func() {
		ctx := context.Background()
		got, err := crdstore.Get[v1alpha1.Controller](ctx, client.store, "mine", "ns")
		if err != nil {
			t.Fatalf("read before power off: %v", err)
		}
		got.Spec.PowerState = "Stopped"
		if err := crdstore.Update(ctx, client.store, got); err != nil {
			t.Fatalf("power off: %v", err)
		}
		for i := 0; i < 5 && client.stsReplicas[prefix] != 0; i++ {
			if _, err := newOwner.Reconcile(ctx, mineRequest); err != nil {
				t.Fatalf("new owner reconcile: %v", err)
			}
		}
		if client.stsReplicas[prefix] != 0 {
			t.Fatalf("new owner did not scale the StatefulSet to 0: replicas = %d", client.stsReplicas[prefix])
		}
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	requireStalledScaleUpFenced(t, client, res, err, prefix)
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	if got.Spec.PowerState != "Stopped" {
		t.Fatalf("powerState = %q, want Stopped", got.Spec.PowerState)
	}
}

// TestReconcile_StalledReplicaDoesNotScaleUpAfterHibernate models a replica
// that reads a Running controller in Provisioning and stalls. The new owner
// hibernates the controller and scales the StatefulSet to 0. The stalled tick
// must conflict instead of writing replicas 1.
func TestReconcile_StalledReplicaDoesNotScaleUpAfterHibernate(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	setA, setB := sharding.NewShardSet(), sharding.NewShardSet()
	setA.Add(shard)
	setB.Add(shard)
	stalled := shardedReplica(client, ring, setA)
	newOwner := shardedReplica(client, ring, setB)
	cr := provisioningStalledController()
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 1}
	client.stsRV = map[string]int{prefix: 1}

	stallAfterFirstRead(t, client, func() {
		ctx := context.Background()
		if _, err := client.SetHibernated(ctx, "mine", "ns", true); err != nil {
			t.Fatalf("new owner hibernate: %v", err)
		}
		for i := 0; i < 5 && client.stsReplicas[prefix] != 0; i++ {
			if _, err := newOwner.Reconcile(ctx, mineRequest); err != nil {
				t.Fatalf("new owner reconcile: %v", err)
			}
		}
		if client.stsReplicas[prefix] != 0 {
			t.Fatalf("new owner did not scale the StatefulSet to 0: replicas = %d", client.stsReplicas[prefix])
		}
	})

	res, err := stalled.Reconcile(context.Background(), mineRequest)
	requireStalledScaleUpFenced(t, client, res, err, prefix)
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	if !got.Status.Hibernated {
		t.Fatal("status.hibernated = false, want true after the hibernation")
	}
}

// TestReconcile_ScaleUpConflictsWhenStatefulSetChangesAfterFence covers a
// StatefulSet that changes between the tick fence and the scale-up write. The
// StatefulSet write must conflict. The tick then requeues and reports no block.
func TestReconcile_ScaleUpConflictsWhenStatefulSetChangesAfterFence(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	ring := sharding.NewRing(256)
	shard := ring.ShardFor("ns/mine")
	set := sharding.NewShardSet()
	set.Add(shard)
	rec := shardedReplica(client, ring, set)
	cr := provisioningStalledController()
	crdstore.MustSeed(client.store, cr)
	prefix := controllerPrefix(cr)
	client.stsReplicas = map[string]int32{prefix: 0}
	client.stsRV = map[string]int{prefix: 0}
	client.afterCreateFence = func() {
		client.stsRV[prefix]++
	}

	res, err := rec.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tickConflictRequeue)
	}
	if client.stsReplicas[prefix] != 0 {
		t.Fatalf("StatefulSet replicas = %d, want 0", client.stsReplicas[prefix])
	}
	got, gerr := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if gerr != nil {
		t.Fatalf("final read: %v", gerr)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == v1alpha1.ConditionReconcileBlocked && c.Status == metav1.ConditionTrue {
			t.Fatalf("ReconcileBlocked=True, want no block after a fenced conflict")
		}
	}
}
