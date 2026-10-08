package sharding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
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
	released *[][]int // each onReleased call, in order
	// heldInHook records, per released shard, whether the held set still
	// had the shard while onReleased ran.
	heldInHook map[int]bool
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
	return h.addManagerWithMeterProvider(identity, ring, noop.NewMeterProvider())
}

// addManagerWithMeterProvider adds a manager that creates its instruments on
// mp. A test that reads a shard metric passes a provider from
// newTestMeterProvider.
func (h *testHarness) addManagerWithMeterProvider(identity string, ring *Ring, mp metric.MeterProvider) *testManagerWrap {
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

	released := make([][]int, 0)
	heldInHook := make(map[int]bool)
	onReleased := func(shards []int) {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]int, len(shards))
		copy(cp, shards)
		released = append(released, cp)
		for _, s := range shards {
			heldInHook[s] = held.Held(s)
		}
	}

	leases := h.cs.CoordinationV1().Leases(h.ns)
	m, err := NewShardManager(mp, leases, h.ns, identity, ring, held, onAcquired, onReleased, slog.Default())
	if err != nil {
		h.t.Fatalf("NewShardManager: %v", err)
	}
	m.clock = h.clock.Now
	m.interval = 1 * time.Millisecond // fast for tests

	w := &testManagerWrap{
		m:          m,
		identity:   identity,
		acquired:   &acquired,
		released:   &released,
		heldInHook: heldInHook,
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

// TestShardManager_RenewalSkipsFreshLeases asserts that the rebalance loop
// only writes a shard-lease renewal once remaining validity drops under 2×
// the rebalance interval.
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

// releasedShards flattens every onReleased call of w into one set.
func releasedShards(w *testManagerWrap) map[int]bool {
	out := make(map[int]bool)
	for _, call := range *w.released {
		for _, s := range call {
			out[s] = true
		}
	}
	return out
}

// TestShardManager_OnReleasedWhenANewcomerTakesShards pins that a replica
// reports each shard that it gives to a new replica.
func TestShardManager_OnReleasedWhenANewcomerTakesShards(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w0 := h.addManager("op-0", ring)
	h.runRebalances(1)
	if len(*w0.released) != 0 {
		t.Fatalf("onReleased calls before a newcomer = %v, want none", *w0.released)
	}

	h.addManager("op-1", ring)
	// op-0 sees the presence lease of op-1 only on the second pass.
	h.runRebalances(2)

	got := releasedShards(w0)
	if len(got) == 0 {
		t.Fatal("op-0 released no shard to the newcomer")
	}
	for s := 0; s < ring.Shards(); s++ {
		if got[s] == w0.m.held.Held(s) {
			t.Fatalf("shard %d: released %v, still held %v; want exactly one", s, got[s], w0.m.held.Held(s))
		}
	}
}

// TestShardManager_OnReleasedWhenTheLeaseIsLost pins that a replica reports
// a shard whose lease another replica holds.
func TestShardManager_OnReleasedWhenTheLeaseIsLost(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

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

	if got := releasedShards(w); len(got) != 1 || !got[0] {
		t.Fatalf("released shards = %v, want only shard 0", got)
	}
}

// TestShardManager_OnReleasedRunsBeforeTheClaim pins that a shard dropped
// and claimed again in one pass is not held while onReleased runs, and that
// onAcquired reports it after that.
func TestShardManager_OnReleasedRunsBeforeTheClaim(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runRebalances(1)
	acquiredBefore := len(*w.acquired)

	if err := h.cs.CoordinationV1().Leases(h.ns).Delete(context.Background(),
		"varroa-shard-0.varroa.dev", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete lease: %v", err)
	}

	h.runRebalances(1)

	if got := releasedShards(w); !got[0] {
		t.Fatalf("released shards = %v, want shard 0", got)
	}
	if w.heldInHook[0] {
		t.Fatal("shard 0 was held while onReleased ran")
	}
	if !w.m.held.Held(0) {
		t.Fatal("op-0 did not claim shard 0 again")
	}
	if len(*w.acquired) != acquiredBefore+1 || (*w.acquired)[acquiredBefore][0] != 0 {
		t.Fatalf("onAcquired calls = %v, want a last call with shard 0", *w.acquired)
	}
}

// TestShardManager_GracefulStopCallsNoReleaseHook pins that the shutdown
// release does not call onReleased.
func TestShardManager_GracefulStopCallsNoReleaseHook(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)

	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w.m.releaseAll(ctx)

	if len(*w.released) != 0 {
		t.Fatalf("onReleased calls after a graceful stop = %v, want none", *w.released)
	}
}

// moveShardOneToANewcomer adds op-1 next to op-0, which holds all shards of
// an 8-shard ring. The assignment gives op-1 the odd shards. op-1 runs one
// pass, so op-0 sees its presence lease on the next pass.
func moveShardOneToANewcomer(t *testing.T, h *testHarness, ring *Ring) (w0, w1 *testManagerWrap) {
	t.Helper()
	w0 = h.addManager("op-0", ring)
	h.runRebalances(1)
	if w0.m.held.Count() != ring.Shards() {
		t.Fatalf("op-0 should hold all shards, got %d", w0.m.held.Count())
	}
	w1 = h.addManager("op-1", ring)
	h.runSingleRebalance("op-1")
	return w0, w1
}

// TestShardManager_ReleaseWaitsForInFlightWork pins that a voluntary release
// keeps the lease while work runs on the shard, and releases it after the
// work ends. Only then can the newcomer claim the shard.
func TestShardManager_ReleaseWaitsForInFlightWork(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w0, w1 := moveShardOneToANewcomer(t, h, ring)
	w0.m.drainWait = 5 * time.Second
	w0.m.cancelWait = time.Second

	ctx, leave, ok := w0.m.held.Enter(context.Background(), 1)
	if !ok {
		t.Fatal("op-0 does not hold shard 1")
	}
	done := make(chan struct{})
	go func() {
		w0.m.rebalance(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the pass ended while work ran on a released shard")
	case <-time.After(100 * time.Millisecond):
	}
	if got := h.leaseHolder("varroa-shard-1.varroa.dev"); got != "op-0" {
		t.Fatalf("shard 1 holder during the drain = %q, want op-0", got)
	}
	h.runSingleRebalance("op-1")
	if w1.m.held.Held(1) {
		t.Fatal("op-1 claimed shard 1 while op-0 work ran on it")
	}

	if ctx.Err() != nil {
		t.Fatalf("the release cancelled the work before its wait ended: %v", ctx.Err())
	}
	leave()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not end after the work ended")
	}
	if got := h.leaseHolder("varroa-shard-1.varroa.dev"); got != "" {
		t.Fatalf("shard 1 holder after the drain = %q, want released", got)
	}
	h.runSingleRebalance("op-1")
	if !w1.m.held.Held(1) {
		t.Fatal("op-1 did not claim shard 1 after the release")
	}
}

// TestShardManager_ReleaseCancelsWorkPastTheDrainWait pins that a release
// cancels work that runs past the drain wait, and releases the lease after
// that work ends.
func TestShardManager_ReleaseCancelsWorkPastTheDrainWait(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w0, _ := moveShardOneToANewcomer(t, h, ring)
	w0.m.drainWait = 10 * time.Millisecond
	w0.m.cancelWait = 2 * time.Second

	ctx, leave, _ := w0.m.held.Enter(context.Background(), 1)
	go func() {
		<-ctx.Done()
		leave()
	}()
	w0.m.rebalance(context.Background())

	if ctx.Err() == nil {
		t.Fatal("the release did not cancel the work")
	}
	if got := h.leaseHolder("varroa-shard-1.varroa.dev"); got != "" {
		t.Fatalf("shard 1 holder = %q, want released", got)
	}
}

// TestShardManager_ReleaseLeavesTheLeaseToExpireWhenWorkIgnoresCancel pins
// that a release keeps the lease of a shard whose work still runs after the
// cancel. The newcomer claims that shard only after the lease expires.
func TestShardManager_ReleaseLeavesTheLeaseToExpireWhenWorkIgnoresCancel(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w0, w1 := moveShardOneToANewcomer(t, h, ring)
	w0.m.drainWait = 10 * time.Millisecond
	w0.m.cancelWait = 10 * time.Millisecond

	_, leave, _ := w0.m.held.Enter(context.Background(), 1)
	defer leave()
	w0.m.rebalance(context.Background())

	if w0.m.held.Held(1) {
		t.Fatal("op-0 still holds shard 1")
	}
	if got := h.leaseHolder("varroa-shard-1.varroa.dev"); got != "op-0" {
		t.Fatalf("shard 1 holder = %q, want op-0 until expiry", got)
	}
	if got := h.leaseHolder("varroa-shard-3.varroa.dev"); got != "" {
		t.Fatalf("shard 3 holder = %q, want released", got)
	}
	h.runSingleRebalance("op-1")
	if w1.m.held.Held(1) {
		t.Fatal("op-1 claimed shard 1 before its lease expired")
	}

	h.clock.advance(31 * time.Second)
	h.runSingleRebalance("op-1")
	if !w1.m.held.Held(1) {
		t.Fatal("op-1 did not claim shard 1 after its lease expired")
	}
}

// TestShardManager_LostLeaseCancelsInFlightWork pins that a replica cancels
// the work on a shard whose lease another replica holds, without a wait.
func TestShardManager_LostLeaseCancelsInFlightWork(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	ctx, leave, _ := w.m.held.Enter(context.Background(), 0)
	defer leave()

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

	if ctx.Err() == nil {
		t.Fatal("the work on a lost shard was not cancelled")
	}
	if got := h.leaseHolder(leaseName); got != "other" {
		t.Fatalf("shard 0 holder = %q, want other", got)
	}
}

// TestShardManager_GracefulStopWaitsForInFlightWork pins that the shutdown
// release keeps a lease while work runs on its shard.
func TestShardManager_GracefulStopWaitsForInFlightWork(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)
	h.runRebalances(1)
	w.m.shutdownDrainWait = 5 * time.Second
	w.m.shutdownCancelWait = time.Second

	_, leave, _ := w.m.held.Enter(context.Background(), 0)
	done := make(chan struct{})
	go func() {
		w.m.releaseAll(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the shutdown release ended while work ran on a shard")
	case <-time.After(100 * time.Millisecond):
	}
	if got := h.leaseHolder("varroa-shard-0.varroa.dev"); got != "op-0" {
		t.Fatalf("shard 0 holder during the drain = %q, want op-0", got)
	}

	leave()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown release did not end after the work ended")
	}
	for i := 0; i < ring.Shards(); i++ {
		if got := h.leaseHolder(fmt.Sprintf("varroa-shard-%d.varroa.dev", i)); got != "" {
			t.Fatalf("shard %d holder = %q, want released", i, got)
		}
	}
}

// failLeaseList makes every List of the leases with the given role label
// fail while *fail is true.
func failLeaseList(h *testHarness, role string, fail *atomic.Bool) {
	h.cs.PrependReactor("list", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		list, ok := action.(k8stesting.ListAction)
		if !ok || !fail.Load() {
			return false, nil, nil
		}
		if list.GetListRestrictions().Labels.String() != "varroa.dev/lease="+role {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("apiserver unreachable")
	})
}

// assertListFailureRevokesPastTheDeadline runs passes while the List of the
// role's leases fails, and checks that the replica holds every shard until the
// deadline and none after the lease duration.
func assertListFailureRevokesPastTheDeadline(t *testing.T, role string) {
	t.Helper()
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	var fail atomic.Bool
	failLeaseList(h, role, &fail)
	w := h.addManager("op-0", ring)
	h.runRebalances(1)
	if got := w.m.held.Count(); got != 8 {
		t.Fatalf("held = %d after the first pass, want 8", got)
	}

	ctx, leave, ok := w.m.held.Enter(context.Background(), 0)
	if !ok {
		t.Fatal("Enter of shard 0 failed")
	}
	defer leave()

	fail.Store(true)
	for i := 0; i < 2; i++ {
		h.clock.advance(10 * time.Second)
		h.runRebalances(1)
		if got := w.m.held.Count(); got != 8 {
			t.Fatalf("held = %d at %ds, want 8 before the deadline", got, 10*(i+1))
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the work on shard 0 was cancelled before the deadline")
	}

	h.clock.advance(10 * time.Second)
	h.runRebalances(1)
	if got := w.m.held.Count(); got != 0 {
		t.Fatalf("held = %d after the List failed past the lease duration, want 0", got)
	}
	if ctx.Err() == nil {
		t.Fatal("the work on an unconfirmed shard was not cancelled")
	}
	if got := releasedShards(w); len(got) != 8 {
		t.Fatalf("onReleased reported %d shards, want 8", len(got))
	}
}

// TestShardManager_ShardListFailureRevokesPastTheDeadline pins that a replica
// that cannot list the shard leases drops its shards and cancels their work.
func TestShardManager_ShardListFailureRevokesPastTheDeadline(t *testing.T) {
	assertListFailureRevokesPastTheDeadline(t, "shard")
}

// TestShardManager_ReplicaListFailureRevokesPastTheDeadline pins the same for
// a replica that cannot list the presence leases.
func TestShardManager_ReplicaListFailureRevokesPastTheDeadline(t *testing.T) {
	assertListFailureRevokesPastTheDeadline(t, "replica")
}

// TestShardManager_StalledListRevokesThroughTheSweep pins that the sweep drops
// the shards at the deadline while a List call blocks the rebalance pass.
func TestShardManager_StalledListRevokesThroughTheSweep(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)
	w.m.sweepInterval = time.Millisecond

	var stall atomic.Bool
	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	// The fake clientset holds its lock while a reactor runs, so the test
	// makes no clientset call while the List blocks.
	h.cs.PrependReactor("list", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !stall.Load() {
			return false, nil, nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block
		return true, nil, fmt.Errorf("apiserver unreachable")
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.m.Start(ctx) }()
	defer func() {
		cancel()
		close(block)
		<-done
	}()

	waitFor(t, "the first pass to claim every shard", func() bool { return w.m.held.Count() == 8 })
	work, leave, ok := w.m.held.Enter(context.Background(), 0)
	if !ok {
		t.Fatal("Enter of shard 0 failed")
	}
	defer leave()

	stall.Store(true)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the rebalance pass did not reach the stalled List")
	}

	h.clock.advance(24 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if got := w.m.held.Count(); got != 8 {
		t.Fatalf("held = %d before the deadline, want 8", got)
	}

	h.clock.advance(time.Second)
	// The sweep drops the shards first and cancels their work after that.
	waitFor(t, "the sweep to drop every shard and cancel its work", func() bool {
		return w.m.held.Count() == 0 && work.Err() != nil
	})
}

// waitFor polls cond for at most five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestShardManager_HealthyRenewalNeverTripsTheDeadline pins that the renewal
// skip never lets a healthy shard reach the revoke deadline.
func TestShardManager_HealthyRenewalNeverTripsTheDeadline(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)
	w.m.interval = 10 * time.Second
	h.runRebalances(1)

	steps := []time.Duration{9 * time.Second, 11 * time.Second, 9900 * time.Millisecond, 10 * time.Second}
	for i := 0; i < 40; i++ {
		h.clock.advance(steps[i%len(steps)])
		w.m.revokeUnconfirmed(context.Background())
		h.runRebalances(1)
		w.m.revokeUnconfirmed(context.Background())
		if got := w.m.held.Count(); got != 8 {
			t.Fatalf("pass %d: held = %d, want 8", i, got)
		}
	}
	if got := releasedShards(w); len(got) != 0 {
		t.Fatalf("onReleased reported %v, want none", got)
	}
}

// TestShardManager_HeldLeaseMarginLeavesRoomForRenewal pins the relation of
// the revoke deadline to the lease duration and the renewal skip.
func TestShardManager_HeldLeaseMarginLeavesRoomForRenewal(t *testing.T) {
	m, err := NewShardManager(noop.NewMeterProvider(), fake.NewSimpleClientset().CoordinationV1().Leases("ns"),
		"ns", "op-0", NewRing(8), NewShardSet(), nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("NewShardManager: %v", err)
	}
	lease := time.Duration(m.leaseDuration) * time.Second
	// A pass renews once less than two intervals remain, and passes come
	// one interval apart.
	healthyMax := lease - 2*m.interval + m.interval
	deadline := m.confirmDeadline()
	if heldLeaseMargin <= 0 {
		t.Fatalf("heldLeaseMargin = %v, want more than zero", heldLeaseMargin)
	}
	if deadline >= lease {
		t.Fatalf("deadline %v, want less than the lease duration %v", deadline, lease)
	}
	if healthyMax >= deadline {
		t.Fatalf("healthy renewal age %v, want less than the deadline %v", healthyMax, deadline)
	}
	if staleSweepInterval >= heldLeaseMargin {
		t.Fatalf("sweep interval %v, want less than the margin %v", staleSweepInterval, heldLeaseMargin)
	}
	if leaseCallTimeout >= deadline {
		t.Fatalf("lease call timeout %v, want less than the deadline %v", leaseCallTimeout, deadline)
	}
}

// TestShardManager_ReclaimsOwnUnexpiredLeaseAfterRevoke pins that a pass
// claims again a shard that the sweep dropped while its lease stayed ours.
func TestShardManager_ReclaimsOwnUnexpiredLeaseAfterRevoke(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	w := h.addManager("op-0", ring)
	h.runRebalances(1)

	h.clock.advance(25 * time.Second)
	w.m.revokeUnconfirmed(context.Background())
	if got := w.m.held.Count(); got != 0 {
		t.Fatalf("held = %d after the sweep, want 0", got)
	}
	if got := h.leaseHolder("varroa-shard-0.varroa.dev"); got != "op-0" {
		t.Fatalf("shard 0 holder = %q, want op-0", got)
	}

	h.runRebalances(1)
	if got := w.m.held.Count(); got != 8 {
		t.Fatalf("held = %d after the next pass, want 8 from the own unexpired leases", got)
	}
	lease, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), "varroa-shard-0.varroa.dev", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(h.clock.Now()) {
		t.Fatalf("shard 0 renewTime = %v, want the reclaim time %v", lease.Spec.RenewTime, h.clock.Now())
	}
}

// TestShardManager_LateClaimResponseIsNotHeld pins that a claim whose
// response comes after the revoke deadline does not hold the shard.
func TestShardManager_LateClaimResponseIsNotHeld(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(1)
	h.cs.PrependReactor("create", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if ok && strings.HasPrefix(create.GetObject().(*coordinationv1.Lease).Name, "varroa-shard-") {
			h.clock.advance(25 * time.Second)
		}
		return false, nil, nil
	})
	w := h.addManager("op-0", ring)
	h.runRebalances(1)
	if w.m.held.Held(0) {
		t.Fatal("a claim confirmed after the deadline holds the shard")
	}
	if len(*w.acquired) != 0 {
		t.Fatalf("onAcquired reported %v, want none", *w.acquired)
	}
}

// TestShardManager_LateRenewalResponseRevokes pins that a renewal whose
// response comes after the revoke deadline drops the shard and its work.
func TestShardManager_LateRenewalResponseRevokes(t *testing.T) {
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(1)
	w := h.addManager("op-0", ring)
	w.m.interval = 10 * time.Second
	h.runRebalances(1)
	work, leave, ok := w.m.held.Enter(context.Background(), 0)
	if !ok {
		t.Fatal("Enter of shard 0 failed")
	}
	defer leave()

	var slow atomic.Bool
	slow.Store(true)
	h.cs.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(k8stesting.UpdateAction)
		if ok && slow.Load() && update.GetObject().(*coordinationv1.Lease).Name == "varroa-shard-0.varroa.dev" {
			slow.Store(false)
			h.clock.advance(25 * time.Second)
		}
		return false, nil, nil
	})
	// Bring the lease into the renewal window.
	h.clock.advance(15 * time.Second)
	h.runRebalances(1)
	if work.Err() == nil {
		t.Fatal("the work on a shard renewed after the deadline was not cancelled")
	}
	if !releasedShards(w)[0] {
		t.Fatal("onReleased did not report shard 0")
	}
}

// blockingListLeases blocks the List of the leases with one role label until
// the call context ends, while block is set.
type blockingListLeases struct {
	coordinationv1client.LeaseInterface
	role    string
	block   *atomic.Bool
	entered chan struct{} // buffered 1
	lastErr atomic.Value  // the error a blocked List returned
}

func (b *blockingListLeases) List(ctx context.Context, opts metav1.ListOptions) (*coordinationv1.LeaseList, error) {
	if b.block.Load() && opts.LabelSelector == "varroa.dev/lease="+b.role {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		b.lastErr.Store(ctx.Err())
		return nil, ctx.Err()
	}
	return b.LeaseInterface.List(ctx, opts)
}

// TestShardManager_BlockedListTimesOutAndTheNextPassRenews pins that a List
// call that blocks ends at the call timeout, and that the next pass renews
// the leases once the call works again.
func TestShardManager_BlockedListTimesOutAndTheNextPassRenews(t *testing.T) {
	for _, role := range []string{"shard", "replica"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
			w := h.addManager("op-0", NewRing(8))
			w.m.interval = 10 * time.Second
			h.runRebalances(1)
			if got := w.m.held.Count(); got != 8 {
				t.Fatalf("held = %d after the first pass, want 8", got)
			}

			blocked := &blockingListLeases{
				LeaseInterface: w.m.leases,
				role:           role,
				block:          &atomic.Bool{},
				entered:        make(chan struct{}, 1),
			}
			blocked.block.Store(true)
			w.m.leases = blocked
			w.m.callTimeout = 20 * time.Millisecond

			// 15 seconds is inside the renewal window and under the deadline.
			h.clock.advance(15 * time.Second)
			done := make(chan struct{})
			go func() {
				defer close(done)
				w.m.rebalance(context.Background())
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the pass did not return within 5 s of a blocked List")
			}
			select {
			case <-blocked.entered:
			default:
				t.Fatal("the blocked List was never called")
			}
			if err, _ := blocked.lastErr.Load().(error); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked List ended with %v, want context.DeadlineExceeded", err)
			}
			if got := w.m.held.Count(); got != 8 {
				t.Fatalf("held = %d after the timed-out pass, want 8 before the deadline", got)
			}

			blocked.block.Store(false)
			h.runRebalances(1)
			if got := w.m.held.Count(); got != 8 {
				t.Fatalf("held = %d after the next pass, want 8", got)
			}
			for i := 0; i < 8; i++ {
				leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", i)
				l, err := h.cs.CoordinationV1().Leases(h.ns).Get(context.Background(), leaseName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get %s: %v", leaseName, err)
				}
				if l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.Equal(h.clock.Now()) {
					t.Fatalf("%s RenewTime = %v, want %v", leaseName, l.Spec.RenewTime, h.clock.Now())
				}
			}
			if got := releasedShards(w); len(got) != 0 {
				t.Fatalf("onReleased reported %d shards, want none", len(got))
			}
		})
	}
}
