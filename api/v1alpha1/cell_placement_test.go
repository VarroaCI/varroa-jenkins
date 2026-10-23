package v1alpha1

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestPlacementCells(t *testing.T) {
	slot := func(n int32) *int32 { return &n }
	cell := func(ns, name string) types.NamespacedName {
		return types.NamespacedName{Namespace: ns, Name: name}
	}
	cases := []struct {
		name      string
		placement *TenantPlacementStatus
		want      []types.NamespacedName
	}{
		{name: "nil placement", placement: nil, want: nil},
		{
			name:      "scalar only",
			placement: &TenantPlacementStatus{Cell: "c0", CellNamespace: "ns"},
			want:      []types.NamespacedName{cell("ns", "c0")},
		},
		{
			name: "active out of order then standbys out of key order",
			placement: &TenantPlacementStatus{Cell: "c0", CellNamespace: "ns", Members: []TenantMemberStatus{
				{Cell: "s-b", CellNamespace: "ns"},
				{Cell: "a2", CellNamespace: "ns", Slot: slot(2)},
				{Cell: "s-a", CellNamespace: "ns"},
				{Cell: "a1", CellNamespace: "ns", Slot: slot(1)},
			}},
			want: []types.NamespacedName{
				cell("ns", "c0"), cell("ns", "a1"), cell("ns", "a2"),
				cell("ns", "s-a"), cell("ns", "s-b"),
			},
		},
		{
			name: "scalar cell repeated as slot 0 appears once",
			placement: &TenantPlacementStatus{Cell: "c0", CellNamespace: "ns", Members: []TenantMemberStatus{
				{Cell: "c0", CellNamespace: "ns", Slot: slot(0)},
			}},
			want: []types.NamespacedName{cell("ns", "c0")},
		},
		{
			name: "member with empty cell name is skipped",
			placement: &TenantPlacementStatus{Cell: "c0", CellNamespace: "ns", Members: []TenantMemberStatus{
				{CellNamespace: "ns", Slot: slot(1)},
			}},
			want: []types.NamespacedName{cell("ns", "c0")},
		},
		{
			name: "same name in two namespaces gives both cells",
			placement: &TenantPlacementStatus{Cell: "c0", CellNamespace: "ns-a", Members: []TenantMemberStatus{
				{Cell: "c0", CellNamespace: "ns-b"},
			}},
			want: []types.NamespacedName{cell("ns-a", "c0"), cell("ns-b", "c0")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlacementCells(tc.placement); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlacementCells() = %v, want %v", got, tc.want)
			}
		})
	}
}
