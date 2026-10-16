package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestTeamHeld(t *testing.T) {
	slot := func(n int32) *int32 { return &n }
	held := func(tenant string) *v1alpha1.CellStatus {
		return &v1alpha1.CellStatus{TenantWarnings: []v1alpha1.TenantWarning{{
			Tenant: tenant, Reason: v1alpha1.TeamReasonItemsHeld, Message: "held",
		}}}
	}
	nonHold := &v1alpha1.CellStatus{TenantWarnings: []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonBundleKeysIgnored, Message: "ignored",
	}}}
	cases := []struct {
		name      string
		placement *v1alpha1.TeamPlacementStatus
		cells     map[string]*v1alpha1.CellStatus
		want      bool
	}{
		{
			name:  "nil placement is not held",
			cells: map[string]*v1alpha1.CellStatus{"cell-1": held("payments")},
			want:  false,
		},
		{
			name:      "slot 0 hold",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns"},
			cells:     map[string]*v1alpha1.CellStatus{"cell-1": held("payments")},
			want:      true,
		},
		{
			name: "non-slot-0 active hold",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns", Members: []v1alpha1.TeamMemberStatus{
				{Cell: "cell-2", CellNamespace: "ns", Slot: slot(1)},
			}},
			cells: map[string]*v1alpha1.CellStatus{"cell-1": {}, "cell-2": held("payments")},
			want:  true,
		},
		{
			name: "standby hold",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns", Members: []v1alpha1.TeamMemberStatus{
				{Cell: "cell-3", CellNamespace: "ns"},
			}},
			cells: map[string]*v1alpha1.CellStatus{"cell-1": {}, "cell-3": held("payments")},
			want:  true,
		},
		{
			name:      "another tenant's hold is not held",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns"},
			cells:     map[string]*v1alpha1.CellStatus{"cell-1": held("other")},
			want:      false,
		},
		{
			name:      "a non-hold reason is not held",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns"},
			cells:     map[string]*v1alpha1.CellStatus{"cell-1": nonHold},
			want:      false,
		},
		{
			name: "a missing member cell is skipped",
			placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "ns", Members: []v1alpha1.TeamMemberStatus{
				{Cell: "cell-2", CellNamespace: "ns", Slot: slot(1)},
				{Cell: "cell-3", CellNamespace: "ns"},
			}},
			cells: map[string]*v1alpha1.CellStatus{"cell-1": {}, "cell-3": held("payments")},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := crdstore.NewFake()
			team := &v1alpha1.Team{
				ObjectMeta: metav1.ObjectMeta{Name: "payments"},
				Status:     v1alpha1.TeamStatus{Placement: tc.placement},
			}
			for name, cs := range tc.cells {
				crdstore.MustSeed(st, &v1alpha1.Controller{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
					Status:     v1alpha1.ControllerStatus{Cell: cs},
				})
			}
			got, err := TeamHeld(context.Background(), st, team)
			if err != nil {
				t.Fatalf("TeamHeld: %v", err)
			}
			if got != tc.want {
				t.Fatalf("TeamHeld = %v, want %v", got, tc.want)
			}
		})
	}
}
