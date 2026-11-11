package status

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/varroaci/varroa-jenkins/internal/election"
)

// LeaseName names the Lease that elects the status writer.
const LeaseName = "varroa-agent-status"

const (
	// tickEvery is the periodic pass. A cache change also triggers a pass.
	tickEvery = 30 * time.Second
	// changeGap bounds how often a cache change triggers a pass.
	changeGap = 5 * time.Second
)

// RunElected runs the ticker while this replica holds the status Lease. A
// replica that is not the leader writes nothing. A replica that loses the Lease
// campaigns for it again. changed carries a signal for each cache change; a nil
// channel disables the change trigger. RunElected returns when ctx ends.
func RunElected(ctx context.Context, kube kubernetes.Interface, ns, id string, t *Ticker, changed <-chan struct{}) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: LeaseName, Namespace: ns},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	return election.Run(ctx, lock, LeaseName, func(lead context.Context) {
		runPasses(lead, t, changed)
	})
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
