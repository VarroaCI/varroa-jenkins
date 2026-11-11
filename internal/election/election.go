// Package election runs the singleton leader leases of the Varroa binaries.
package election

import (
	"context"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// The lease timings for the singleton leases: the operator manager lease, the
// BFF RBAC federation lease, and the Tenant agent status lease. The shard
// leases of the operator use their own timings and are not covered here.
//
// Two constraints fix the values:
//   - RenewDeadline must exceed 25.4 seconds. A link loss of up to about
//     25 seconds stalls API requests until the Linux retransmit timer fires
//     (200 ms, doubled six times). A shorter deadline ends the leader on a
//     short network fault.
//   - The cluster heartbeat interval (30 seconds), plus LeaseDuration, plus two
//     jittered RetryPeriods must stay under the cluster entry TTL (90 seconds).
const (
	LeaseDuration = 40 * time.Second
	RenewDeadline = 30 * time.Second
	RetryPeriod   = 2 * time.Second
)

type timing struct {
	lease, renew, retry time.Duration
}

var defaultTiming = timing{lease: LeaseDuration, renew: RenewDeadline, retry: RetryPeriod}

// Run campaigns for the lease name on lock. While this replica holds the
// lease, lead runs. Its context ends when the lease is lost or when ctx ends.
// Run campaigns again only after lead returns, so lead never runs twice at
// once. lead must return only after its context ends. A lead that returns
// while the lease is held keeps the lease without doing work. Run releases the
// lease when ctx ends. It returns an error only for an invalid configuration.
func Run(ctx context.Context, lock resourcelock.Interface, name string, lead func(context.Context)) error {
	return run(ctx, lock, name, defaultTiming, lead)
}

func run(ctx context.Context, lock resourcelock.Interface, name string, t timing, lead func(context.Context)) error {
	for ctx.Err() == nil {
		leading := make(chan context.Context, 1)
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            name,
			LeaseDuration:   t.lease,
			RenewDeadline:   t.renew,
			RetryPeriod:     t.retry,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(c context.Context) { leading <- c },
				OnStoppedLeading: func() {},
			},
		})
		if err != nil {
			return err
		}
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			le.Run(ctx)
		}()
		select {
		case c := <-leading:
			lead(c)
			<-ran
		case <-ran:
		}
	}
	return nil
}
