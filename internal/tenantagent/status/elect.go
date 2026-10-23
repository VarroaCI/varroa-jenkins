package status

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaseName names the Lease that elects the status writer.
const LeaseName = "varroa-agent-status"

const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
	// tickEvery is the periodic pass. A cache change also triggers a pass.
	tickEvery = 30 * time.Second
	// changeGap bounds how often a cache change triggers a pass.
	changeGap = 5 * time.Second
)

// RunElected runs the ticker while this replica holds the status Lease. A
// replica that is not the leader writes nothing. changed carries a signal for
// each cache change; a nil channel disables the change trigger. RunElected
// returns when ctx ends.
func RunElected(ctx context.Context, kube kubernetes.Interface, ns, id string, t *Ticker, changed <-chan struct{}) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: LeaseName, Namespace: ns},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Name:            LeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lead context.Context) {
				runPasses(lead, t, changed)
			},
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		return err
	}
	le.Run(ctx)
	return nil
}

// runPasses ticks on a timer and on each cache change, at most once per changeGap.
func runPasses(ctx context.Context, t *Ticker, changed <-chan struct{}) {
	periodic := time.NewTicker(tickEvery)
	defer periodic.Stop()
	var last time.Time
	pass := func() {
		_ = t.Tick(ctx)
		last = time.Now()
	}
	pass()
	for {
		select {
		case <-ctx.Done():
			return
		case <-periodic.C:
			pass()
		case <-changed:
			if time.Since(last) < changeGap {
				time.Sleep(changeGap - time.Since(last))
			}
			pass()
		}
	}
}
