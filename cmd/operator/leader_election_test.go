package main

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/varroaci/varroa-jenkins/internal/election"
)

func TestWithLeaderElection(t *testing.T) {
	o := withLeaderElection(manager.Options{HealthProbeBindAddress: ":8090"}, "varroa-system")
	if !o.LeaderElection {
		t.Error("LeaderElection = false, want true")
	}
	if o.LeaderElectionID != "varroa-operator.varroa.dev" {
		t.Errorf("LeaderElectionID = %q", o.LeaderElectionID)
	}
	if o.LeaderElectionNamespace != "varroa-system" {
		t.Errorf("LeaderElectionNamespace = %q", o.LeaderElectionNamespace)
	}
	if o.LeaseDuration == nil || *o.LeaseDuration != election.LeaseDuration {
		t.Errorf("LeaseDuration = %v, want %v", o.LeaseDuration, election.LeaseDuration)
	}
	if o.RenewDeadline == nil || *o.RenewDeadline != election.RenewDeadline {
		t.Errorf("RenewDeadline = %v, want %v", o.RenewDeadline, election.RenewDeadline)
	}
	if o.RetryPeriod == nil || *o.RetryPeriod != election.RetryPeriod {
		t.Errorf("RetryPeriod = %v, want %v", o.RetryPeriod, election.RetryPeriod)
	}
	if !o.LeaderElectionReleaseOnCancel {
		t.Error("LeaderElectionReleaseOnCancel = false, want true")
	}
	if o.HealthProbeBindAddress != ":8090" {
		t.Errorf("HealthProbeBindAddress = %q, want :8090", o.HealthProbeBindAddress)
	}
}
