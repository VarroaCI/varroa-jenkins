package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// clusterDrainRunner deletes every Controller CR on this cluster while the
// lifecycle state is "draining". It flips the state to "drained" when zero
// controllers remain. Leader-elected: exactly one replica runs the loop.
type clusterDrainRunner struct {
	clusterName string
	lifecycle   *controller.LifecycleStore
	client      controller.CommandCRUDClient
	store       crdstore.Backend
	activity    *activity.Publisher // may be nil; guard every publish
	beatNow     func()              // clusterHeartbeatRunner.BeatNow
	logger      *slog.Logger
}

func (r *clusterDrainRunner) NeedLeaderElection() bool { return true }

func (r *clusterDrainRunner) Start(ctx context.Context) error {
	// Immediate tick on election, then every 15s.
	r.tick(ctx)
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *clusterDrainRunner) tick(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	st, err := r.lifecycle.Load(bctx)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("cluster drain runner: lifecycle load failed", "error", err)
		}
		return
	}
	if st.State != bus.ClusterStateDraining {
		return
	}

	ctrls, err := crdstore.List[v1alpha1.Controller](bctx, r.store, "", "")
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("cluster drain runner: list controllers failed", "error", err)
		}
		return
	}

	if len(ctrls) == 0 {
		if err := r.lifecycle.SetDrained(bctx); err != nil {
			if r.logger != nil {
				r.logger.Warn("cluster drain runner: set drained failed", "error", err)
			}
			return
		}
		if r.activity != nil {
			r.activity.Publish(activity.Event{
				Type:    "cluster.drain.completed",
				Source:  "operator",
				Message: "cluster drained",
			})
		}
		if r.beatNow != nil {
			r.beatNow()
		}
		return
	}

	for _, cr := range ctrls {
		if cr.DeletionTimestamp == nil {
			if err := crdstore.Delete[v1alpha1.Controller](bctx, r.store, cr.Name, cr.Namespace); err != nil {
				if r.logger != nil {
					r.logger.Warn("cluster drain runner: delete controller failed",
						"controller", cr.Namespace+"/"+cr.Name, "error", err)
				}
				continue
			}
		}
	}
}
