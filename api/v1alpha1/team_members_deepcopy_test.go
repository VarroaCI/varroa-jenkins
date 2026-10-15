package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTeamDeepCopyIsolatesMembers mutates a deep copy of a Team that holds
// two active members and one standby. DeepCopy for this kind is hand-written,
// so this round trip is the only acceptance of the new slice and the new slot
// pointer.
func TestTeamDeepCopyIsolatesMembers(t *testing.T) {
	slot0, slot1 := int32(0), int32(1)
	members := int32(2)
	standby := int32(1)
	orig := Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: TeamSpec{
			Pipelines: &TeamPipelinesSpec{
				Profile: "lts", Members: &members, Standby: &standby,
			},
		},
		Status: TeamStatus{
			Placement: &TeamPlacementStatus{
				Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 3,
				Members: []TeamMemberStatus{
					{Cell: "cell-a", CellNamespace: "cells-ns", Role: TeamMemberRoleActive, Slot: &slot0, Applied: true},
					{Cell: "cell-b", CellNamespace: "cells-ns", Role: TeamMemberRoleActive, Slot: &slot1, Applied: true},
					{Cell: "cell-c", CellNamespace: "cells-ns", Role: TeamMemberRoleStandby},
				},
			},
		},
	}

	copied := orig.DeepCopy()
	copied.Status.Placement.Members[0].Cell = "changed"
	*copied.Status.Placement.Members[1].Slot = 7
	copied.Status.Placement.Members = append(copied.Status.Placement.Members, TeamMemberStatus{Cell: "cell-d"})
	*copied.Spec.Pipelines.Members = 9
	*copied.Spec.Pipelines.Standby = 9

	if orig.Status.Placement.Members[0].Cell != "cell-a" {
		t.Errorf("the copy changed the original member cell: %+v", orig.Status.Placement.Members[0])
	}
	if got := *orig.Status.Placement.Members[1].Slot; got != 1 {
		t.Errorf("the copy changed the original slot: %d, want 1", got)
	}
	if len(orig.Status.Placement.Members) != 3 {
		t.Errorf("the copy changed the original length: %d, want 3", len(orig.Status.Placement.Members))
	}
	if got := *orig.Spec.Pipelines.Members; got != 2 {
		t.Errorf("the copy changed spec.pipelines.members: %d, want 2", got)
	}
	if got := *orig.Spec.Pipelines.Standby; got != 1 {
		t.Errorf("the copy changed spec.pipelines.standby: %d, want 1", got)
	}
	if orig.Status.Placement.Members[2].Slot != nil {
		t.Error("a standby carries a slot")
	}
}

// TestEffectiveMembersAndStandby asserts the defaults a Go caller sees when
// the apiserver has applied no CRD default.
func TestEffectiveMembersAndStandby(t *testing.T) {
	var nilSpec *TeamPipelinesSpec
	if got := nilSpec.EffectiveMembers(); got != 1 {
		t.Errorf("a nil spec answers %d members, want 1", got)
	}
	if got := nilSpec.EffectiveStandby(); got != 0 {
		t.Errorf("a nil spec answers %d standby, want 0", got)
	}
	set := int32(4)
	spec := &TeamPipelinesSpec{Members: &set, Standby: &set}
	if got := spec.EffectiveMembers(); got != 4 {
		t.Errorf("the spec answers %d members, want 4", got)
	}
	if got := spec.EffectiveStandby(); got != 4 {
		t.Errorf("the spec answers %d standby, want 4", got)
	}
	zero := int32(0)
	if got := (&TeamPipelinesSpec{Members: &zero}).EffectiveMembers(); got != 1 {
		t.Errorf("a zero members value answers %d, want 1", got)
	}
}
