package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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
