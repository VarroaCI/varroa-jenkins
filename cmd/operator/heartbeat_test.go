package main

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

func TestBuildClusterInfo(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	activeState := controller.LifecycleState{State: bus.ClusterStateActive}

	t.Run("zero controllers", func(t *testing.T) {
		info := buildClusterInfo("core", "v1.0.0", "v1.30.0", nil, now, activeState)
		if info.Name != "core" {
			t.Errorf("Name = %q, want %q", info.Name, "core")
		}
		if info.OperatorVersion != "v1.0.0" {
			t.Errorf("OperatorVersion = %q, want %q", info.OperatorVersion, "v1.0.0")
		}
		if info.K8sVersion != "v1.30.0" {
			t.Errorf("K8sVersion = %q, want %q", info.K8sVersion, "v1.30.0")
		}
		if info.ControllerCount != 0 {
			t.Errorf("ControllerCount = %d, want 0", info.ControllerCount)
		}
		if info.ConnectedCount != 0 {
			t.Errorf("ConnectedCount = %d, want 0", info.ConnectedCount)
		}
		if !info.LastHeartbeat.Equal(now) {
			t.Errorf("LastHeartbeat = %v, want %v", info.LastHeartbeat, now)
		}
		if info.State != bus.ClusterStateActive {
			t.Errorf("State = %q, want %q", info.State, bus.ClusterStateActive)
		}
	})

	t.Run("mixed connected status", func(t *testing.T) {
		ctrls := []*v1alpha1.Controller{
			{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected}},
			{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseRunning}},
			{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected}},
			{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseProvisioning}},
		}
		info := buildClusterInfo("dev-cluster", "abc1234", "v1.31.0", ctrls, now, activeState)
		if info.ControllerCount != 4 {
			t.Errorf("ControllerCount = %d, want 4", info.ControllerCount)
		}
		if info.ConnectedCount != 2 {
			t.Errorf("ConnectedCount = %d, want 2", info.ConnectedCount)
		}
		if info.Name != "dev-cluster" {
			t.Errorf("Name = %q, want %q", info.Name, "dev-cluster")
		}
	})

	t.Run("field passthrough", func(t *testing.T) {
		ctrls := []*v1alpha1.Controller{
			{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected}},
		}
		info := buildClusterInfo("test-cluster", "deadbeef", "v1.32.0", ctrls, now, activeState)
		if info.Name != "test-cluster" {
			t.Errorf("Name = %q, want %q", info.Name, "test-cluster")
		}
		if info.OperatorVersion != "deadbeef" {
			t.Errorf("OperatorVersion = %q, want %q", info.OperatorVersion, "deadbeef")
		}
		if info.K8sVersion != "v1.32.0" {
			t.Errorf("K8sVersion = %q, want %q", info.K8sVersion, "v1.32.0")
		}
		if info.ControllerCount != 1 {
			t.Errorf("ControllerCount = %d, want 1", info.ControllerCount)
		}
		if info.ConnectedCount != 1 {
			t.Errorf("ConnectedCount = %d, want 1", info.ConnectedCount)
		}
	})

	t.Run("utc timestamp", func(t *testing.T) {
		ctrls := []*v1alpha1.Controller{}
		info := buildClusterInfo("core", "dev", "v1.30.0", ctrls, now, activeState)
		if info.LastHeartbeat.Location().String() != "UTC" {
			t.Errorf("LastHeartbeat location = %v, want UTC", info.LastHeartbeat.Location())
		}
	})

	t.Run("stamps draining state and drainStartedAt", func(t *testing.T) {
		ds := time.Now().UTC().Truncate(time.Second)
		drainingState := controller.LifecycleState{
			State:          bus.ClusterStateDraining,
			DrainStartedAt: &ds,
		}
		info := buildClusterInfo("dev-cluster", "abc", "v1.30.0", nil, now, drainingState)
		if info.State != bus.ClusterStateDraining {
			t.Errorf("State = %q, want %q", info.State, bus.ClusterStateDraining)
		}
		if info.DrainStartedAt == nil {
			t.Fatal("DrainStartedAt should be set")
		}
		if !info.DrainStartedAt.Equal(ds) {
			t.Errorf("DrainStartedAt = %v, want %v", info.DrainStartedAt, ds)
		}
	})

	t.Run("stamps active state with nil DrainStartedAt", func(t *testing.T) {
		info := buildClusterInfo("dev-cluster", "abc", "v1.30.0", nil, now, activeState)
		if info.State != bus.ClusterStateActive {
			t.Errorf("State = %q, want %q", info.State, bus.ClusterStateActive)
		}
		if info.DrainStartedAt != nil {
			t.Errorf("DrainStartedAt = %v, want nil", info.DrainStartedAt)
		}
	})

	t.Run("defaults empty state to active", func(t *testing.T) {
		emptyState := controller.LifecycleState{}
		info := buildClusterInfo("core", "abc", "v1.30.0", nil, now, emptyState)
		if info.State != bus.ClusterStateActive {
			t.Errorf("State = %q, want %q", info.State, bus.ClusterStateActive)
		}
	})
}
