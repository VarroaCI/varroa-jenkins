package sharding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeClock is a controllable clock for testing.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// testShardManagerHarness runs one or more rebalance passes for a set of managers
// sharing the same fake clientset.
type testHarness struct {
	t        *testing.T
	ns       string
	cs       *fake.Clientset
	clock    *fakeClock
	managers []*testManagerWrap
}

type testManagerWrap struct {
	m        *ShardManager
	identity string
	acquired *[][]int // pointer to shared slice so onAcquired closure mutations are visible
}

func newHarness(t *testing.T, startTime time.Time) *testHarness {
	t.Helper()
	cs := fake.NewSimpleClientset()
	clock := newFakeClock(startTime)
	return &testHarness{
		t:     t,
		ns:    "varroa-system",
		cs:    cs,
		clock: clock,
	}
}

func (h *testHarness) addManager(identity string, ring *Ring) *testManagerWrap {
	held := NewShardSet()
	acquired := make([][]int, 0)
	var mu sync.Mutex
	onAcquired := func(shards []int) {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]int, len(shards))
		copy(cp, shards)
		acquired = append(acquired, cp)
	}

	leases := h.cs.CoordinationV1().Leases(h.ns)
	m := NewShardManager(leases, h.ns, identity, ring, held, onAcquired, slog.Default())
	m.clock = h.clock.Now
	m.interval = 1 * time.Millisecond // fast for tests

	w := &testManagerWrap{
		m:        m,
		identity: identity,
		acquired: &acquired,
	}
	h.managers = append(h.managers, w)
	return w
}

func (h *testHarness) runRebalances(count int) {
	for i := 0; i < count; i++ {
		for _, w := range h.managers {
			w.m.rebalance(context.Background())
		}
	}
}

func (h *testHarness) runSingleRebalance(identity string) {
	for _, w := range h.managers {
		if w.identity == identity {
			w.m.rebalance(context.Background())
			return
		}
	}
}

func (h *testHarness) leaseExists(name string) bool {
	_, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func (h *testHarness) leaseHolder(name string) string {
	l, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func TestShardManager_SingleReplica(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)

	h.runRebalances(1)

	if w.m.held.Count() != ring.Shards() {
		t.Fatalf("expected to hold %d shards, got %d", ring.Shards(), w.m.held.Count())
	}

	presenceName := "varroa-replica-op-0.varroa.dev"
	if !h.leaseExists(presenceName) {
		t.Fatal("presence lease should exist")
	}
}

func TestShardManager_TwoReplicas(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w0 := h.addManager("op-0", ring)
	w1 := h.addManager("op-1", ring)

	h.runRebalances(2)

	allShards := make(map[int]bool)
	held0 := make(map[int]bool)
	for _, s := range w0.m.held.Shards() {
		held0[s] = true
		allShards[s] = true
	}
	held1 := make(map[int]bool)
	for _, s := range w1.m.held.Shards() {
		if held0[s] {
			t.Fatalf("shard %d held by both replicas", s)
		}
		held1[s] = true
		allShards[s] = true
	}

	if len(allShards) != ring.Shards() {
		t.Fatalf("expected %d total shards across replicas, got %d", ring.Shards(), len(allShards))
	}

	diff := w0.m.held.Count() - w1.m.held.Count()
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("shard count diff > 1: %d vs %d", w0.m.held.Count(), w1.m.held.Count())
	}
}

func TestShardManager_Newcomer(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w0 := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w0.m.held.Count() != ring.Shards() {
		t.Fatalf("op-0 should hold all shards, got %d", w0.m.held.Count())
	}

	w1 := h.addManager("op-1", ring)
	h.runRebalances(1)

	if w1.m.held.Count() > 0 {
		t.Fatalf("B should not have acquired shards yet (A's leases fresh), got %d", w1.m.held.Count())
	}

	h.runRebalances(1)

	total := w0.m.held.Count() + w1.m.held.Count()
	if total != ring.Shards() {
		t.Fatalf("expected %d total shards, got %d (A: %d, B: %d)", ring.Shards(), total, w0.m.held.Count(), w1.m.held.Count())
	}

	for _, s := range w0.m.held.Shards() {
		if w1.m.held.Held(s) {
			t.Fatalf("shard %d held by both", s)
		}
	}

	if w1.m.held.Count() == 0 {
		t.Fatal("B should have acquired some shards")
	}
}

func TestShardManager_ExpiryTakeover(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w0 := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w0.m.held.Count() != ring.Shards() {
		t.Fatalf("op-0 should hold all shards, got %d", w0.m.held.Count())
	}

	h.clock.advance(31 * time.Second)

	w1 := h.addManager("op-1", ring)
	// Only run B's rebalance — A doesn't run, so its leases stay expired.
	h.runSingleRebalance("op-1")

	if w1.m.held.Count() != ring.Shards() {
		t.Fatalf("B should have claimed all expired shards, got %d", w1.m.held.Count())
	}
}

func TestShardManager_NeverSteal(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	identity := "other"
	duration := int32(30)
	now := h.clock.Now()
	for i := 0; i < ring.Shards(); i++ {
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", i)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      leaseName,
				Namespace: h.ns,
				Labels: map[string]string{
					"varroa.dev/component": "shard",
					"varroa.dev/lease":     "shard",
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &identity,
				LeaseDurationSeconds: &duration,
				AcquireTime:          &metav1.MicroTime{Time: now},
				RenewTime:            &metav1.MicroTime{Time: now},
			},
		}
		if _, err := h.cs.CoordinationV1().Leases(h.ns).Create(context.Background(), lease, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to pre-create shard lease: %v", err)
		}
	}

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w.m.held.Count() > 0 {
		t.Fatalf("op-0 should not steal unexpired foreign leases, but holds %d shards", w.m.held.Count())
	}
}

func TestShardManager_RenewalConflict(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w.m.held.Count() != ring.Shards() {
		t.Fatalf("op-0 should hold all shards, got %d", w.m.held.Count())
	}

	leaseName := "varroa-shard-0.varroa.dev"
	lease, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), leaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get lease: %v", err)
	}
	other := "other"
	lease.Spec.HolderIdentity = &other
	if _, err := h.cs.CoordinationV1().Leases(h.ns).Update(context.Background(), lease, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to mutate lease: %v", err)
	}

	h.runRebalances(1)

	if w.m.held.Held(0) {
		t.Fatal("op-0 should have removed shard 0 after renewal conflict")
	}
}

func TestShardManager_GracefulStop(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w.m.held.Count() != ring.Shards() {
		t.Fatalf("op-0 should hold all shards, got %d", w.m.held.Count())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w.m.releaseAll(ctx)

	for i := 0; i < ring.Shards(); i++ {
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", i)
		holder := h.leaseHolder(leaseName)
		if holder != "" {
			t.Fatalf("shard %d lease should be released, holder=%q", i, holder)
		}
	}

	presenceName := "varroa-replica-op-0.varroa.dev"
	if h.leaseExists(presenceName) {
		t.Fatal("presence lease should be deleted after graceful stop")
	}
}

func TestShardManager_OnAcquired(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runSingleRebalance("op-0")

	if len(*w.acquired) < 1 {
		t.Fatal("onAcquired should have been called at least once")
	}
	lastCall := (*w.acquired)[len(*w.acquired)-1]
	if len(lastCall) != ring.Shards() {
		t.Fatalf("expected %d newly acquired shards in last call, got %d", ring.Shards(), len(lastCall))
	}

	heldSet := make(map[int]bool)
	for _, s := range w.m.held.Shards() {
		heldSet[s] = true
	}
	for _, s := range lastCall {
		if !heldSet[s] {
			t.Fatalf("acquired shard %d not in held set", s)
		}
	}
}

func TestShardManager_LeaseObjectShape(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(4)

	h.addManager("op-0", ring)
	h.runRebalances(1)

	for i := 0; i < ring.Shards(); i++ {
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", i)
		lease, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), leaseName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get lease %s: %v", leaseName, err)
		}
		if lease.Labels["varroa.dev/component"] != "shard" {
			t.Errorf("shard lease missing varroa.dev/component=shard label")
		}
		if lease.Labels["varroa.dev/lease"] != "shard" {
			t.Errorf("shard lease missing varroa.dev/lease=shard label")
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "op-0" {
			t.Errorf("shard lease holder mismatch")
		}
		if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds != 30 {
			t.Errorf("shard lease duration mismatch")
		}
		if lease.Spec.RenewTime == nil {
			t.Errorf("shard lease renewTime is nil")
		}
		if lease.Spec.AcquireTime == nil {
			t.Errorf("shard lease acquireTime is nil")
		}
	}

	presenceName := "varroa-replica-op-0.varroa.dev"
	presenceLease, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), presenceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get presence lease: %v", err)
	}
	if presenceLease.Labels["varroa.dev/component"] != "shard" {
		t.Errorf("presence lease missing varroa.dev/component=shard label")
	}
	if presenceLease.Labels["varroa.dev/lease"] != "replica" {
		t.Errorf("presence lease missing varroa.dev/lease=replica label")
	}
	if presenceLease.Spec.HolderIdentity == nil || *presenceLease.Spec.HolderIdentity != "op-0" {
		t.Errorf("presence lease holder mismatch")
	}
	if presenceLease.Spec.LeaseDurationSeconds == nil || *presenceLease.Spec.LeaseDurationSeconds != 30 {
		t.Errorf("presence lease duration mismatch")
	}
}

func TestShardManager_ConflictRetry(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(4)

	conflict := true
	h.cs.PrependReactor("update", "leases", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if conflict {
			conflict = false
			return true, nil, apierrors.NewConflict(coordinationv1.Resource("leases"), "varroa-shard-0.varroa.dev", fmt.Errorf("conflict"))
		}
		h.cs.ReactionChain = h.cs.ReactionChain[1:]
		return false, nil, nil
	})

	w := h.addManager("op-0", ring)
	h.runRebalances(2)

	if w.m.held.Count() == 0 {
		t.Fatal("should have recovered from conflict")
	}
}

func TestShardManager_ClockSkewTolerance(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	identity := "other"
	duration := int32(30)
	now := h.clock.Now()
	skewedTime := now.Add(-10 * time.Second)
	for i := 0; i < ring.Shards(); i++ {
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", i)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      leaseName,
				Namespace: h.ns,
				Labels: map[string]string{
					"varroa.dev/component": "shard",
					"varroa.dev/lease":     "shard",
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &identity,
				LeaseDurationSeconds: &duration,
				AcquireTime:          &metav1.MicroTime{Time: skewedTime},
				RenewTime:            &metav1.MicroTime{Time: skewedTime},
			},
		}
		if _, err := h.cs.CoordinationV1().Leases(h.ns).Create(context.Background(), lease, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to pre-create shard lease: %v", err)
		}
	}

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	if w.m.held.Count() > 0 {
		t.Fatalf("op-0 should not steal still-fresh leases (10s old, 30s duration), holds %d", w.m.held.Count())
	}
}

// TestShardManager_RenewalSkipsFreshLeases pins the renewal cadence (issue
// #280): the rebalance loop only writes a shard-lease renewal once remaining
// validity drops under 2× the rebalance interval.
func TestShardManager_RenewalSkipsFreshLeases(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	ring := NewRing(4)
	w := h.addManager("replica-a", ring)
	w.m.interval = 10 * time.Second // realistic cadence: threshold = 20s of a 30s lease

	// Acquire all shards.
	h.runRebalances(1)
	if w.m.held.Count() != 4 {
		t.Fatalf("expected 4 shards held, got %d", w.m.held.Count())
	}

	var shardUpdates int
	h.cs.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ua, ok := action.(k8stesting.UpdateAction); ok {
			if l, ok := ua.GetObject().(*coordinationv1.Lease); ok && strings.HasPrefix(l.Name, "varroa-shard-") {
				shardUpdates++
			}
		}
		return false, nil, nil
	})

	// Leases aged 5s: remaining 25s > 20s threshold — no renewal writes.
	h.clock.advance(5 * time.Second)
	h.runRebalances(1)
	if shardUpdates != 0 {
		t.Fatalf("expected no shard renewals while leases are fresh, got %d", shardUpdates)
	}

	// Leases aged 15s: remaining 15s < 20s threshold — renewals written.
	h.clock.advance(10 * time.Second)
	h.runRebalances(1)
	if shardUpdates != 4 {
		t.Fatalf("expected 4 shard renewals once under the threshold, got %d", shardUpdates)
	}
}
