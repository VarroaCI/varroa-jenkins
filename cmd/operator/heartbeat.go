package main

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// clusterHeartbeats is the counter for successfully written member-cluster
// heartbeats. Registered once at package init.
var clusterHeartbeats metric.Int64Counter

func init() {
	meter := otel.Meter("varroa-operator")
	var err error
	clusterHeartbeats, err = meter.Int64Counter("varroa.cluster.heartbeats",
		metric.WithDescription("Cluster membership heartbeats successfully written"))
	if err != nil {
		panic("failed to register varroa.cluster.heartbeats counter: " + err.Error())
	}
}

// clusterHeartbeatRunner publishes this cluster's membership heartbeat into
// the varroa_clusters KV bucket. Leader-elected: exactly one replica per
// cluster heartbeats (same posture as operatorCommandRunner / C1 aux runnables).
type clusterHeartbeatRunner struct {
	clusterName string // from VARROA_CLUSTER_NAME (C4), default "core"
	version     string // main.version ldflag value
	clustersKV  *bus.KV
	client      *controller.ClientsetClient
	store       crdstore.Backend
	lifecycle   *controller.LifecycleStore
	beatNow     chan struct{} // buffered size 1; non-blocking send
	logger      *slog.Logger
}

func (r *clusterHeartbeatRunner) NeedLeaderElection() bool { return true }

// BeatNow triggers an out-of-band heartbeat (non-blocking).
func (r *clusterHeartbeatRunner) BeatNow() {
	select {
	case r.beatNow <- struct{}{}:
	default:
	}
}

func (r *clusterHeartbeatRunner) Start(ctx context.Context) error {
	r.beat(ctx) // first beat immediately on election
	t := time.NewTicker(bus.ClusterHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.beat(ctx)
		case <-r.beatNow:
			r.beat(ctx)
		}
	}
}

// beat performs a single heartbeat cycle with a 10s context timeout.
// On controller-list error the beat is skipped entirely (safe: TTL tolerates 2 misses).
// On server-version error the beat still proceeds with empty k8sVersion.
func (r *clusterHeartbeatRunner) beat(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ctrls, err := crdstore.List[v1alpha1.Controller](bctx, r.store, "", "")
	if err != nil {
		r.logger.Warn("cluster heartbeat: list controllers failed, skipping beat", "error", err)
		return
	}

	connected := 0
	for _, c := range ctrls {
		if c.Status.Phase == v1alpha1.ControllerPhaseConnected {
			connected++
		}
	}

	k8sVersion, err := r.client.ServerVersion(bctx)
	if err != nil {
		r.logger.Warn("cluster heartbeat: server version failed, using empty", "error", err)
		k8sVersion = ""
	}

	// Load lifecycle state for drain status. Fresh read is the mirror of record;
	// on error, fall back to cached State() with a warning.
	lifecycleState, loadErr := r.lifecycle.Load(bctx)
	if loadErr != nil {
		r.logger.Warn("cluster heartbeat: lifecycle load failed, using cached", "error", loadErr)
		lifecycleState = r.lifecycle.State()
	}

	info := buildClusterInfo(r.clusterName, r.version, k8sVersion, ctrls, time.Now().UTC(), lifecycleState)

	if err := bus.PutClusterHeartbeat(r.clustersKV, info); err != nil {
		r.logger.Warn("cluster heartbeat: put failed", "error", err)
		return
	}
	clusterHeartbeats.Add(ctx, 1)
}

// buildClusterInfo assembles a ClusterInfo struct from the heartbeat components.
// Extracted for testability; ctrls may be nil/empty.
func buildClusterInfo(name, version, k8sVersion string, ctrls []*v1alpha1.Controller, now time.Time, st controller.LifecycleState) bus.ClusterInfo {
	connected := 0
	for _, c := range ctrls {
		if c.Status.Phase == v1alpha1.ControllerPhaseConnected {
			connected++
		}
	}
	state := st.State
	if state == "" {
		state = bus.ClusterStateActive
	}
	return bus.ClusterInfo{
		Name:            name,
		OperatorVersion: version,
		K8sVersion:      k8sVersion,
		ControllerCount: len(ctrls),
		ConnectedCount:  connected,
		LastHeartbeat:   now,
		State:           state,
		DrainStartedAt:  st.DrainStartedAt,
	}
}
