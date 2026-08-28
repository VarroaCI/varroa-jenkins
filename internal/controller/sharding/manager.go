package sharding

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ShardManager claims per-shard Kubernetes Leases so operator replicas divide
// Controller CRs among themselves (active/active). It runs on every replica.
type ShardManager struct {
	leases     coordinationv1client.LeaseInterface // clientset.CoordinationV1().Leases(namespace)
	namespace  string                              // operator namespace
	identity   string                              // pod name (os.Hostname())
	ring       *Ring
	held       *ShardSet          // shared with the Reconciler (read side)
	onAcquired func(shards []int) // Reconciler.EnqueueShards; called outside the CAS loop
	logger     *slog.Logger

	interval      time.Duration    // rebalance period, default 10s
	leaseDuration int32            // seconds, default 30
	clock         func() time.Time // test seam

	// metrics
	shardsHeld metric.Int64ObservableGauge
	handoffs   metric.Int64Counter
}

// NewShardManager creates a new ShardManager.
func NewShardManager(leases coordinationv1client.LeaseInterface, namespace, identity string,
	ring *Ring, held *ShardSet, onAcquired func([]int), logger *slog.Logger) *ShardManager {
	m := &ShardManager{
		leases:        leases,
		namespace:     namespace,
		identity:      identity,
		ring:          ring,
		held:          held,
		onAcquired:    onAcquired,
		logger:        logger,
		interval:      10 * time.Second,
		leaseDuration: 30,
		clock:         time.Now,
	}

	// Register metrics.
	meter := otel.Meter("varroa-operator")

	shardsHeld, err := meter.Int64ObservableGauge(
		"varroa.operator.shards.held",
		metric.WithDescription("Number of shards currently held by this replica"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(int64(m.held.Count()))
			return nil
		}),
	)
	if err != nil {
		logger.Warn("failed to register shards-held gauge", "error", err)
	} else {
		m.shardsHeld = shardsHeld
	}

	handoffs, err := meter.Int64Counter(
		"varroa.operator.shard.handoffs",
		metric.WithDescription("Number of shard handoff transitions"),
	)
	if err != nil {
		logger.Warn("failed to register shard handoffs counter", "error", err)
	} else {
		m.handoffs = handoffs
	}

	return m
}

// addHandoff records a handoff transition; nil-safe so a failed metric
// registration can never panic the rebalance loop.
func (m *ShardManager) addHandoff(ctx context.Context, direction string) {
	if m.handoffs == nil {
		return
	}
	m.handoffs.Add(ctx, 1, metric.WithAttributes(attribute.String("direction", direction)))
}

// NeedLeaderElection returns false so the ShardManager runs on every replica
// regardless of leader status.
func (m *ShardManager) NeedLeaderElection() bool { return false }

// Start runs the rebalance loop until ctx is cancelled, then releases all shards.
func (m *ShardManager) Start(ctx context.Context) error {
	m.logger.Info("shard manager starting", "identity", m.identity, "shardCount", m.ring.Shards())

	// Run initial rebalance immediately.
	m.rebalance(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("shard manager shutting down, releasing all shards")
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			m.releaseAll(releaseCtx)
			return nil
		case <-ticker.C:
			m.rebalance(ctx)
		}
	}
}

// rebalance implements the rebalance algorithm from design §1.2.
func (m *ShardManager) rebalance(ctx context.Context) {
	logger := m.logger

	// Step 1: Upsert presence lease.
	m.upsertPresence(ctx)

	// Step 2: List presence leases to discover live replicas.
	replicaLeases, err := m.leases.List(ctx, metav1.ListOptions{
		LabelSelector: "varroa.dev/lease=replica",
	})
	if err != nil {
		logger.Warn("failed to list replica presence leases", "error", err)
		return
	}

	// Build live replica set: unexpired leases only.
	now := m.clock()
	liveReplicas := make([]string, 0)
	for _, l := range replicaLeases.Items {
		if !isLeaseExpired(&l, now, m.leaseDuration) {
			holder := l.Spec.HolderIdentity
			if holder != nil && *holder != "" {
				liveReplicas = append(liveReplicas, *holder)
			}
		}
	}
	// Ensure self is included even if our presence lease somehow expired.
	if !slices.Contains(liveReplicas, m.identity) {
		liveReplicas = append(liveReplicas, m.identity)
	}

	// Step 3: Compute desired assignment.
	desired := make(map[int]bool)
	assignment := m.ring.Assign(liveReplicas)
	for _, s := range assignment.Shards[m.identity] {
		desired[s] = true
	}

	// Step 4: List shard ownership leases.
	shardLeases, err := m.leases.List(ctx, metav1.ListOptions{
		LabelSelector: "varroa.dev/lease=shard",
	})
	if err != nil {
		logger.Warn("failed to list shard leases", "error", err)
		return
	}
	shardLeaseMap := make(map[int]*coordinationv1.Lease, len(shardLeases.Items))
	for i := range shardLeases.Items {
		l := &shardLeases.Items[i]
		var shardNum int
		if _, err := fmt.Sscanf(l.Name, "varroa-shard-%d.varroa.dev", &shardNum); err == nil {
			shardLeaseMap[shardNum] = l
		}
	}

	// Step 5: Renew held shards — drop any where we lost the lock.
	for _, shard := range m.held.Shards() {
		lease, exists := shardLeaseMap[shard]
		if !exists {
			m.held.Remove(shard)
			m.addHandoff(ctx, "released")
			continue
		}
		holder := lease.Spec.HolderIdentity
		if holder == nil || *holder != m.identity {
			m.held.Remove(shard)
			m.addHandoff(ctx, "released")
			continue
		}
		// Skip the apiserver write while the lease is still fresh: renewal is
		// only needed once the remaining validity drops under two rebalance
		// intervals (halves the steady-state write rate at the default 30s
		// duration / 10s interval).
		if lease.Spec.RenewTime != nil {
			remaining := lease.Spec.RenewTime.Add(time.Duration(m.leaseDuration) * time.Second).Sub(m.clock())
			if remaining > 2*m.interval {
				continue
			}
		}
		// Try to renew our lease.
		updatedLease := lease.DeepCopy()
		now := m.clock()
		updatedLease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		updatedLease.Spec.LeaseDurationSeconds = &m.leaseDuration
		if _, err := m.leases.Update(ctx, updatedLease, metav1.UpdateOptions{}); err != nil {
			m.held.Remove(shard)
			m.addHandoff(ctx, "released")
			continue
		}
	}

	// Step 6: Release shards we hold but no longer desire.
	for _, shard := range m.held.Shards() {
		if desired[shard] {
			continue
		}
		m.held.Remove(shard)
		// Release the lease.
		lease, exists := shardLeaseMap[shard]
		if !exists {
			// Lease already gone; nothing to release.
			m.addHandoff(ctx, "released")
			continue
		}
		m.releaseLease(ctx, lease)
		m.addHandoff(ctx, "released")
	}

	// Step 7: Acquire desired but not held shards.
	newly := make([]int, 0)
	for shard := range desired {
		if m.held.Held(shard) {
			continue
		}
		lease, exists := shardLeaseMap[shard]
		claimable := !exists || isLeaseReleased(lease) || isLeaseExpired(lease, now, m.leaseDuration)
		if !claimable {
			continue
		}

		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", shard)
		now := m.clock()

		if exists {
			// Try to update the existing lease.
			updatedLease := lease.DeepCopy()
			updatedLease.Spec = m.heldLeaseSpec(now, true)
			if _, err := m.leases.Update(ctx, updatedLease, metav1.UpdateOptions{}); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
					// Lost the race, retry next pass.
					continue
				}
				logger.Warn("failed to claim shard lease", "shard", shard, "error", err)
				continue
			}
		} else {
			// Create a new lease.
			newLease := m.buildLease(leaseName, "shard", now, true)
			if _, err := m.leases.Create(ctx, newLease, metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Lost the race, retry next pass.
					continue
				}
				logger.Warn("failed to create shard lease", "shard", shard, "error", err)
				continue
			}
		}

		m.held.Add(shard)
		newly = append(newly, shard)
		m.addHandoff(ctx, "acquired")
	}

	// Step 8: Notify on newly acquired shards.
	if len(newly) > 0 && m.onAcquired != nil {
		m.onAcquired(newly)
	}
}

// heldLeaseSpec returns a LeaseSpec holding a lease as this replica, renewed
// at now. withAcquireTime stamps AcquireTime (shard claims); presence leases omit it.
func (m *ShardManager) heldLeaseSpec(now time.Time, withAcquireTime bool) coordinationv1.LeaseSpec {
	identity := m.identity
	duration := m.leaseDuration
	spec := coordinationv1.LeaseSpec{
		HolderIdentity:       &identity,
		LeaseDurationSeconds: &duration,
		RenewTime:            &metav1.MicroTime{Time: now},
	}
	if withAcquireTime {
		spec.AcquireTime = &metav1.MicroTime{Time: now}
	}
	return spec
}

// buildLease constructs a new lease held by this replica. role is the
// varroa.dev/lease label value ("shard" or "replica").
func (m *ShardManager) buildLease(name, role string, now time.Time, withAcquireTime bool) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.namespace,
			Labels: map[string]string{
				"varroa.dev/component": "shard",
				"varroa.dev/lease":     role,
			},
		},
		Spec: m.heldLeaseSpec(now, withAcquireTime),
	}
}

// upsertPresence creates or updates the replica presence lease.
func (m *ShardManager) upsertPresence(ctx context.Context) {
	leaseName := "varroa-replica-" + m.identity + ".varroa.dev"
	now := m.clock()

	lease := m.buildLease(leaseName, "replica", now, false)
	_, err := m.leases.Create(ctx, lease, metav1.CreateOptions{})
	if err == nil {
		return
	}
	if !apierrors.IsAlreadyExists(err) {
		m.logger.Warn("failed to create presence lease", "lease", leaseName, "error", err)
		return
	}

	// Lease exists; update it.
	existing, err := m.leases.Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		m.logger.Warn("failed to get presence lease for update", "lease", leaseName, "error", err)
		return
	}
	existing.Spec = m.heldLeaseSpec(now, false)
	if _, err := m.leases.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		m.logger.Warn("failed to update presence lease", "lease", leaseName, "error", err)
	}
}

// releasedSpec clears the holder on a lease in place, marking it claimable.
func (m *ShardManager) releasedSpec(l *coordinationv1.Lease) {
	empty := ""
	l.Spec.HolderIdentity = &empty
	l.Spec.RenewTime = nil
	l.Spec.AcquireTime = nil
	l.Spec.LeaseDurationSeconds = &m.leaseDuration
}

// releaseLease clears the holder on a shard lease. The lease object usually
// comes from a List and may be stale, so a resourceVersion conflict is
// expected churn: re-get and retry once, and log any remaining failure at
// debug — release is best-effort because expiry makes an unreleased lease
// claimable anyway.
func (m *ShardManager) releaseLease(ctx context.Context, lease *coordinationv1.Lease) {
	updated := lease.DeepCopy()
	m.releasedSpec(updated)
	_, err := m.leases.Update(ctx, updated, metav1.UpdateOptions{})
	if err == nil {
		return
	}
	if !apierrors.IsConflict(err) {
		m.logger.Warn("failed to release shard lease, will expire", "lease", lease.Name, "error", err)
		return
	}
	fresh, getErr := m.leases.Get(ctx, lease.Name, metav1.GetOptions{})
	if getErr != nil {
		m.logger.Debug("shard lease release retry get failed, will expire", "lease", lease.Name, "error", getErr)
		return
	}
	if fresh.Spec.HolderIdentity == nil || *fresh.Spec.HolderIdentity != m.identity {
		// Someone else holds (or released) it now; nothing to release.
		return
	}
	m.releasedSpec(fresh)
	if _, err := m.leases.Update(ctx, fresh, metav1.UpdateOptions{}); err != nil {
		m.logger.Debug("shard lease release retry failed, will expire", "lease", lease.Name, "error", err)
	}
}

// releaseAll releases all held shards and deletes the presence lease.
func (m *ShardManager) releaseAll(ctx context.Context) {
	for _, shard := range m.held.Shards() {
		m.held.Remove(shard)
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", shard)
		// Try to release by setting empty holderIdentity.
		existing, err := m.leases.Get(ctx, leaseName, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				m.logger.Warn("failed to get shard lease for release", "lease", leaseName, "error", err)
			}
			continue
		}
		m.releaseLease(ctx, existing)
	}

	// Delete presence lease.
	presenceName := "varroa-replica-" + m.identity + ".varroa.dev"
	if err := m.leases.Delete(ctx, presenceName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			m.logger.Warn("failed to delete presence lease on shutdown", "lease", presenceName, "error", err)
		}
	}
}

// isLeaseExpired returns true if the lease has expired (renewTime + duration < now).
func isLeaseExpired(lease *coordinationv1.Lease, now time.Time, durationSec int32) bool {
	if lease.Spec.RenewTime == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(durationSec) * time.Second)
	return now.After(expiry)
}

// isLeaseReleased returns true if the lease has no holder identity.
func isLeaseReleased(lease *coordinationv1.Lease) bool {
	return lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == ""
}
