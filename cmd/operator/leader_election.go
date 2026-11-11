package main

import (
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/varroaci/varroa-jenkins/internal/election"
)

// leaderElectionID names the manager lease. Only the replica that holds it runs
// the leader-only work.
const leaderElectionID = "varroa-operator.varroa.dev"

// withLeaderElection sets the manager lease on o. It uses the shared singleton
// timings and releases the lease when the manager stops. The release is safe
// because main returns at once after the manager stops.
func withLeaderElection(o manager.Options, namespace string) manager.Options {
	o.LeaderElection = true
	o.LeaderElectionID = leaderElectionID
	o.LeaderElectionNamespace = namespace
	o.LeaseDuration = ptr.To(election.LeaseDuration)
	o.RenewDeadline = ptr.To(election.RenewDeadline)
	o.RetryPeriod = ptr.To(election.RetryPeriod)
	o.LeaderElectionReleaseOnCancel = true
	return o
}
