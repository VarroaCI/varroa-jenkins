package sharding

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// releaseDrainWait and releaseCancelWait limit how long a rebalance pass
// waits for the work on the shards that it releases. Their sum must stay at
// or under half the rebalance interval. The pass renews its leases before
// the wait, so the next pass then starts on time and no renewal is late.
const (
	releaseDrainWait  = 4 * time.Second
	releaseCancelWait = 1 * time.Second
)

// shutdownDrainWait, shutdownCancelWait, and shutdownReleaseTimeout limit the
// shutdown release. Their sum must stay under the 30-second graceful shutdown
// timeout of the controller-runtime manager.
const (
	shutdownDrainWait      = 10 * time.Second
	shutdownCancelWait     = 2 * time.Second
	shutdownReleaseTimeout = 2 * time.Second
)

// heldLeaseMargin is how long before the lease expiry a replica drops a shard
// that it could not confirm. Other replicas judge the expiry by their own wall
// clock, and a cancel needs time to stop a send that is in flight. The margin
// gives that time. The revoke deadline is the lease duration minus this
// margin: 25 seconds of the 30-second lease. A healthy replica renews a shard
// at most about 20 seconds after the last renewal, because a pass skips the
// renewal while more than two intervals of the lease remain. So the deadline
// must stay above 20 seconds, and the margin must stay at or under 5 seconds.
const heldLeaseMargin = 5 * time.Second

// staleSweepInterval is the period of the sweep that revokes unconfirmed
// shards. The sweep runs apart from the rebalance pass, because one pass can
// wait longer than the margin: each lease call can take up to leaseCallTimeout,
// and a pass can wait in the release drain.
const staleSweepInterval = 1 * time.Second

// leaseCallTimeout limits each lease API call of the shard manager. It is below
// the 25 s revoke deadline, so a hung call ends before the deadline. A shorter
// bound would drop every shard on a slow apiserver. It is a context per call:
// watches share rest.Config, so rest.Config.Timeout must stay unset.
const leaseCallTimeout = 20 * time.Second

// ShardManager claims per-shard Kubernetes Leases so operator replicas divide
// Controller CRs among themselves (active/active). It runs on every replica.
type ShardManager struct {
	leases     coordinationv1client.LeaseInterface // clientset.CoordinationV1().Leases(namespace)
	namespace  string                              // operator namespace
	identity   string                              // pod name (os.Hostname())
	ring       *Ring
	held       *ShardSet          // shared with the Reconciler (read side)
	onAcquired func(shards []int) // Reconciler.EnqueueShards; called outside the CAS loop
	// onReleased receives each group of revoked or released shards: the
	// shards that one rebalance pass dropped, or the shards that one stale
	// sweep revoked. A pass calls it before it claims shards, so a shard
	// dropped and claimed again in one pass starts from a clean state. A pass
	// calls it after the release waited for the work on those shards. The
	// sweep can call it while a pass runs. The shutdown release does not call
	// it.
	onReleased func(shards []int)
	logger     *slog.Logger

	interval      time.Duration    // rebalance period, default 10s
	leaseDuration int32            // seconds, default 30
	clock         func() time.Time // test seam

	// The drain waits. A zero value selects the matching constant. Tests
	// set short values.
	drainWait          time.Duration
	cancelWait         time.Duration
	shutdownDrainWait  time.Duration
	shutdownCancelWait time.Duration
	// sweepInterval is the stale sweep period. A zero value selects
	// staleSweepInterval.
	sweepInterval time.Duration

	// callTimeout limits each lease API call. A zero value selects
	// leaseCallTimeout.
	callTimeout time.Duration

	// metrics
	shardsHeld metric.Int64ObservableGauge
	handoffs   metric.Int64Counter
}

// NewShardManager creates a new ShardManager. It creates the shard
// instruments on the "varroa-operator" meter of mp and returns an error when
// an instrument cannot be created.
func NewShardManager(mp metric.MeterProvider, leases coordinationv1client.LeaseInterface, namespace, identity string,
	ring *Ring, held *ShardSet, onAcquired, onReleased func([]int), logger *slog.Logger) (*ShardManager, error) {
	m := &ShardManager{
		leases:        leases,
		namespace:     namespace,
		identity:      identity,
		ring:          ring,
		held:          held,
		onAcquired:    onAcquired,
		onReleased:    onReleased,
		logger:        logger,
		interval:      10 * time.Second,
		leaseDuration: 30,
		clock:         time.Now,
	}

	meter := mp.Meter("varroa-operator")

	shardsHeld, err := meter.Int64ObservableGauge(
		"varroa.operator.shards.held",
		metric.WithDescription("Number of shards currently held by this replica"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(int64(m.held.Count()))
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create shards-held gauge: %w", err)
	}
	m.shardsHeld = shardsHeld

	handoffs, err := meter.Int64Counter(
		"varroa.operator.shard.handoffs",
		metric.WithDescription("Number of shard handoff transitions"),
	)
	if err != nil {
		return nil, fmt.Errorf("create shard handoffs counter: %w", err)
	}
	m.handoffs = handoffs

	return m, nil
}

// addHandoff records a handoff transition. A manager that a test builds
// without NewShardManager has no counter, so addHandoff accepts a nil counter.
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

	var sweep sync.WaitGroup
	sweep.Add(1)
	go func() {
		defer sweep.Done()
		m.sweepLoop(ctx)
	}()

	// Run initial rebalance immediately.
	m.rebalance(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The sweep must stop before the release, so it does not revoke
			// shards while the release drains them.
			sweep.Wait()
			m.logger.Info("shard manager shutting down, releasing all shards")
			releaseCtx, cancel := context.WithTimeout(context.Background(),
				orDefault(m.shutdownDrainWait, shutdownDrainWait)+
					orDefault(m.shutdownCancelWait, shutdownCancelWait)+shutdownReleaseTimeout)
			defer cancel()
			m.releaseAll(releaseCtx)
			return nil
		case <-ticker.C:
			m.rebalance(ctx)
		}
	}
}

// sweepLoop revokes unconfirmed shards until ctx ends.
func (m *ShardManager) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(orDefault(m.sweepInterval, staleSweepInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.revokeUnconfirmed(ctx)
		}
	}
}

// confirmDeadline is the longest time that a held shard can go without a
// confirmed lease write.
func (m *ShardManager) confirmDeadline() time.Duration {
	return time.Duration(m.leaseDuration)*time.Second - heldLeaseMargin
}

// revokeUnconfirmed revokes every held shard whose last confirmed lease write
// started confirmDeadline or more ago. Another replica can claim such a shard
// soon, so the work on it is cancelled at once, without a wait. It calls
// onReleased once with the revoked shards. That can run at the same time as a
// rebalance pass: the hook reads the held set when it runs, and a shard that
// a pass claims again is enqueued again through onAcquired.
func (m *ShardManager) revokeUnconfirmed(ctx context.Context) {
	revoked := m.held.RevokeStale(m.clock(), m.confirmDeadline())
	if len(revoked) == 0 {
		return
	}
	m.logger.Warn("dropping shards whose leases were not confirmed before the deadline",
		"shards", revoked, "deadline", m.confirmDeadline())
	for range revoked {
		m.addHandoff(ctx, "released")
	}
	if m.onReleased != nil {
		m.onReleased(revoked)
	}
}

// revokeInPass revokes a shard in a rebalance pass and records the release.
// It records nothing when the stale sweep already dropped the shard, so one
// release is reported once.
func (m *ShardManager) revokeInPass(ctx context.Context, shard int, released *[]int) {
	if m.held.Revoke(shard) {
		*released = append(*released, shard)
		m.addHandoff(ctx, "released")
	}
}

// rebalance implements the rebalance algorithm from design §1.2.
func (m *ShardManager) rebalance(ctx context.Context) {
	logger := m.logger

	// Step 1: Upsert presence lease.
	m.upsertPresence(ctx)

	// Step 2: List presence leases to discover live replicas.
	replicaLeases, err := m.listLeases(ctx, "replica")
	if err != nil {
		logger.Warn("failed to list replica presence leases", "error", err)
		m.revokeUnconfirmed(ctx)
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
	shardLeases, err := m.listLeases(ctx, "shard")
	if err != nil {
		logger.Warn("failed to list shard leases", "error", err)
		m.revokeUnconfirmed(ctx)
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

	// Step 5: Renew held shards — drop any where we lost the lock. Another
	// replica can hold such a lease already, so Revoke cancels the work on
	// the shard at once. A wait cannot keep that shard exclusive.
	released := make([]int, 0)
	for _, shard := range m.held.Shards() {
		lease, exists := shardLeaseMap[shard]
		if !exists {
			m.revokeInPass(ctx, shard, &released)
			continue
		}
		holder := lease.Spec.HolderIdentity
		if holder == nil || *holder != m.identity {
			m.revokeInPass(ctx, shard, &released)
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
		if err := m.updateLease(ctx, updatedLease); err != nil {
			m.revokeInPass(ctx, shard, &released)
			continue
		}
		// A renewal whose response came after the deadline does not prove
		// that the lease is still ours: another replica can have claimed it.
		if m.clock().Sub(now) >= m.confirmDeadline() {
			m.revokeInPass(ctx, shard, &released)
			continue
		}
		m.held.Confirm(shard, now)
	}

	// Step 6: Release shards we hold but no longer desire. The lease is
	// still ours, so the work on these shards may end first. The release
	// of a lease waits for that work, so the next holder never writes at
	// the same time as this replica.
	undesired := make([]int, 0)
	for _, shard := range m.held.Shards() {
		if !desired[shard] {
			undesired = append(undesired, shard)
		}
	}
	// The stale sweep can drop an undesired shard before the drain. The sweep
	// reported that shard and left its lease to expire, so the pass skips it.
	dropped := make([]int, 0)
	busy := make(map[int]bool)
	if len(undesired) > 0 {
		var stillBusy []int
		dropped, stillBusy = m.held.DrainHeld(undesired,
			orDefault(m.drainWait, releaseDrainWait), orDefault(m.cancelWait, releaseCancelWait))
		for _, shard := range stillBusy {
			busy[shard] = true
		}
	}
	for _, shard := range dropped {
		released = append(released, shard)
		m.addHandoff(ctx, "released")
		if busy[shard] {
			logger.Warn("work on the shard still runs after the cancel; the lease must expire", "shard", shard)
			continue
		}
		lease, exists := shardLeaseMap[shard]
		if !exists {
			// Lease already gone; nothing to release.
			continue
		}
		m.releaseLease(ctx, lease)
	}

	// Notify on released shards before any claim. The hook then sees a
	// dropped shard as not held, even when step 7 claims it again.
	if len(released) > 0 && m.onReleased != nil {
		m.onReleased(released)
	}

	// Step 7: Acquire desired but not held shards.
	newly := make([]int, 0)
	for shard := range desired {
		if m.held.Held(shard) {
			continue
		}
		lease, exists := shardLeaseMap[shard]
		// A lease that this replica still holds is claimable too: a stale
		// sweep can drop a shard whose lease stays ours. The Update below
		// carries the listed resourceVersion, so a write by another replica
		// after the List makes it fail.
		claimable := !exists || isLeaseReleased(lease) || isLeaseHeldBy(lease, m.identity) ||
			isLeaseExpired(lease, now, m.leaseDuration)
		if !claimable {
			continue
		}

		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", shard)
		now := m.clock()

		if exists {
			// Try to update the existing lease.
			updatedLease := lease.DeepCopy()
			updatedLease.Spec = m.heldLeaseSpec(now, true)
			if err := m.updateLease(ctx, updatedLease); err != nil {
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
			if err := m.createLease(ctx, newLease); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Lost the race, retry next pass.
					continue
				}
				logger.Warn("failed to create shard lease", "shard", shard, "error", err)
				continue
			}
		}

		// A claim whose response came after the deadline can hold an expired
		// lease that another replica claimed in between. Do not hold it.
		if m.clock().Sub(now) >= m.confirmDeadline() {
			logger.Warn("shard claim confirmed after the deadline; not holding the shard", "shard", shard)
			continue
		}
		m.held.Add(shard)
		m.held.Confirm(shard, now)
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
	err := m.createLease(ctx, lease)
	if err == nil {
		return
	}
	if !apierrors.IsAlreadyExists(err) {
		m.logger.Warn("failed to create presence lease", "lease", leaseName, "error", err)
		return
	}

	// Lease exists; update it.
	existing, err := m.getLease(ctx, leaseName)
	if err != nil {
		m.logger.Warn("failed to get presence lease for update", "lease", leaseName, "error", err)
		return
	}
	existing.Spec = m.heldLeaseSpec(now, false)
	if err := m.updateLease(ctx, existing); err != nil {
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
	err := m.updateLease(ctx, updated)
	if err == nil {
		return
	}
	if !apierrors.IsConflict(err) {
		m.logger.Warn("failed to release shard lease, will expire", "lease", lease.Name, "error", err)
		return
	}
	fresh, getErr := m.getLease(ctx, lease.Name)
	if getErr != nil {
		m.logger.Debug("shard lease release retry get failed, will expire", "lease", lease.Name, "error", getErr)
		return
	}
	if fresh.Spec.HolderIdentity == nil || *fresh.Spec.HolderIdentity != m.identity {
		// Someone else holds (or released) it now; nothing to release.
		return
	}
	m.releasedSpec(fresh)
	if err := m.updateLease(ctx, fresh); err != nil {
		m.logger.Debug("shard lease release retry failed, will expire", "lease", lease.Name, "error", err)
	}
}

// releaseAll waits for the work on all held shards, releases the shards, and
// deletes the presence lease. It keeps the lease of a shard whose work still
// runs after the cancel, so that lease expires.
func (m *ShardManager) releaseAll(ctx context.Context) {
	shards := m.held.Shards()
	busy := make(map[int]bool)
	for _, shard := range m.held.Drain(shards,
		orDefault(m.shutdownDrainWait, shutdownDrainWait), orDefault(m.shutdownCancelWait, shutdownCancelWait)) {
		busy[shard] = true
	}
	for _, shard := range shards {
		if busy[shard] {
			m.logger.Warn("work on the shard still runs after the cancel; the lease must expire", "shard", shard)
			continue
		}
		leaseName := fmt.Sprintf("varroa-shard-%d.varroa.dev", shard)
		// Try to release by setting empty holderIdentity.
		existing, err := m.getLease(ctx, leaseName)
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
	if err := m.deleteLease(ctx, presenceName); err != nil {
		if !apierrors.IsNotFound(err) {
			m.logger.Warn("failed to delete presence lease on shutdown", "lease", presenceName, "error", err)
		}
	}
}

// callCtx returns a context for one lease API call. The call ends at the
// lease call timeout, or earlier when ctx ends.
func (m *ShardManager) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, orDefault(m.callTimeout, leaseCallTimeout))
}

// listLeases lists the leases of one role under the lease call timeout.
func (m *ShardManager) listLeases(ctx context.Context, role string) (*coordinationv1.LeaseList, error) {
	ctx, cancel := m.callCtx(ctx)
	defer cancel()
	return m.leases.List(ctx, metav1.ListOptions{LabelSelector: "varroa.dev/lease=" + role})
}

// getLease gets one lease under the lease call timeout.
func (m *ShardManager) getLease(ctx context.Context, name string) (*coordinationv1.Lease, error) {
	ctx, cancel := m.callCtx(ctx)
	defer cancel()
	return m.leases.Get(ctx, name, metav1.GetOptions{})
}

// createLease creates one lease under the lease call timeout.
func (m *ShardManager) createLease(ctx context.Context, lease *coordinationv1.Lease) error {
	ctx, cancel := m.callCtx(ctx)
	defer cancel()
	_, err := m.leases.Create(ctx, lease, metav1.CreateOptions{})
	return err
}

// updateLease updates one lease under the lease call timeout.
func (m *ShardManager) updateLease(ctx context.Context, lease *coordinationv1.Lease) error {
	ctx, cancel := m.callCtx(ctx)
	defer cancel()
	_, err := m.leases.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

// deleteLease deletes one lease under the lease call timeout.
func (m *ShardManager) deleteLease(ctx context.Context, name string) error {
	ctx, cancel := m.callCtx(ctx)
	defer cancel()
	return m.leases.Delete(ctx, name, metav1.DeleteOptions{})
}

// orDefault returns d, or def when d is zero.
func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// isLeaseExpired returns true if the lease has expired (renewTime + duration < now).
func isLeaseExpired(lease *coordinationv1.Lease, now time.Time, durationSec int32) bool {
	if lease.Spec.RenewTime == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(durationSec) * time.Second)
	return now.After(expiry)
}

// isLeaseHeldBy returns true if identity holds the lease.
func isLeaseHeldBy(lease *coordinationv1.Lease, identity string) bool {
	return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == identity
}

// isLeaseReleased returns true if the lease has no holder identity.
func isLeaseReleased(lease *coordinationv1.Lease) bool {
	return lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == ""
}
