package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestShardGate_Reconcile_NonOwned verifies that Reconcile on a non-owned
// controller returns immediately without touching the client.
func TestShardGate_Reconcile_NonOwned(t *testing.T) {
	ring := sharding.NewRing(8)
	held := sharding.NewShardSet()

	// Hold only shard 0.
	held.Add(0)

	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetShardOwnership(ring, held)

	// Create a controller whose key hashes to a shard we don't hold.
	shard := ring.ShardFor("ns-other/test-ctrl")
	if held.Held(shard) {
		t.Fatalf("test setup: shard %d should not be held", shard)
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-other", Name: "test-ctrl"}}
	result, err := rec.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected empty result for non-owned controller, got %+v", result)
	}

	// Verify the client wasn't called (no status patches recorded).
	if len(client.statuses) > 0 {
		t.Fatalf("expected zero client calls for non-owned controller, got %d", len(client.statuses))
	}
}

// TestShardGate_Reconcile_NilGate verifies that a nil shard gate (unwired)
// reconciles as today — the client is called for an owned controller.
func TestShardGate_Reconcile_NilGate(t *testing.T) {
	client := newTestClientWithBundle()
	client.controllers = []*v1alpha1.Controller{
		testController("test-ctrl", "test-ns", v1alpha1.ControllerPhaseConnected),
	}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}
	rec := newTestReconciler(client)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
	_, err := rec.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestShardGate_EnqueueShards verifies that EnqueueShards enqueues exactly the
// CRs hashing to the given shards onto reconcileEvents.
func TestShardGate_EnqueueShards(t *testing.T) {
	ring := sharding.NewRing(8)
	held := sharding.NewShardSet()

	client := newTestClient()
	rec := newTestReconciler(client)
	rec.shardRing = ring
	rec.shardSet = held

	// Create a few controllers with different keys.
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-c", Namespace: "ns-c"}},
	}
	client.controllers = controllers
	for _, c := range controllers {
		crdstore.MustSeed(client.store, c)
	}

	// Determine which shard each controller falls into.
	shardToKeys := make(map[int][]string)
	for _, cr := range controllers {
		s := ring.ShardFor(cr.Namespace + "/" + cr.Name)
		shardToKeys[s] = append(shardToKeys[s], cr.Namespace+"/"+cr.Name)
	}

	// Pick one shard arbitrarily and enqueue it.
	var targetShard int
	var expectedKeys []string
	for s, keys := range shardToKeys {
		targetShard = s
		expectedKeys = keys
		break
	}

	rec.EnqueueShards([]int{targetShard})

	// Drain reconcileEvents and collect keys.
	collected := make(map[string]bool)
	for i := 0; i < len(expectedKeys); i++ {
		select {
		case ev := <-rec.reconcileEvents:
			key := ev.Object.GetNamespace() + "/" + ev.Object.GetName()
			collected[key] = true
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for reconcile event")
		}
	}

	// Verify no extra events.
	select {
	case <-rec.reconcileEvents:
		t.Fatal("unexpected extra reconcile event")
	default:
	}

	for _, key := range expectedKeys {
		if !collected[key] {
			t.Fatalf("expected key %s not enqueued", key)
		}
	}
}

// TestShardGate_EnqueueShards_SkipsNonMatching verifies that EnqueueShards only
// enqueues CRs whose shard is in the given set, skipping others.
func TestShardGate_EnqueueShards_SkipsNonMatching(t *testing.T) {
	ring := sharding.NewRing(8)
	held := sharding.NewShardSet()

	client := newTestClient()
	rec := newTestReconciler(client)
	rec.shardRing = ring
	rec.shardSet = held

	// Single controller.
	c := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "my-ns"},
	}
	client.controllers = []*v1alpha1.Controller{c}
	crdstore.MustSeed(client.store, c)

	targetShard := ring.ShardFor("my-ns/my-ctrl")
	// Enqueue a different shard so the controller is NOT enqueued.
	otherShard := (targetShard + 1) % ring.Shards()

	rec.EnqueueShards([]int{otherShard})

	// No events should appear.
	select {
	case ev := <-rec.reconcileEvents:
		t.Fatalf("unexpected event for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
	case <-time.After(100 * time.Millisecond):
		// Expected — no events.
	}
}

// TestShardGate_EnqueueShards_NilRing does nothing when ring is nil.
func TestShardGate_EnqueueShards_NilRing(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// shardRing is nil — EnqueueShards should return immediately.
	rec.EnqueueShards([]int{0, 1, 2})
}
