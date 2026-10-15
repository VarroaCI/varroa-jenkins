package v1alpha1

import "testing"

func TestCellHoldsTenant(t *testing.T) {
	held := func(tenant, reason string) *Controller {
		return &Controller{Status: ControllerStatus{Cell: &CellStatus{TenantWarnings: []TenantWarning{
			{Tenant: tenant, Reason: reason},
		}}}}
	}
	cases := []struct {
		name   string
		cr     *Controller
		tenant string
		want   bool
	}{
		{name: "nil controller", cr: nil, tenant: "payments", want: false},
		{name: "nil cell status", cr: &Controller{}, tenant: "payments", want: false},
		{name: "items held for the tenant", cr: held("payments", TeamReasonItemsHeld), tenant: "payments", want: true},
		{name: "items held for another tenant", cr: held("other", TeamReasonItemsHeld), tenant: "payments", want: false},
		{name: "keys ignored for the tenant", cr: held("payments", TeamReasonBundleKeysIgnored), tenant: "payments", want: false},
		{name: "empty tenant", cr: held("", TeamReasonItemsHeld), tenant: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CellHoldsTenant(tc.cr, tc.tenant); got != tc.want {
				t.Fatalf("CellHoldsTenant = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCell(t *testing.T) {
	cases := []struct {
		name string
		cr   *Controller
		want bool
	}{
		{name: "nil controller", cr: nil, want: false},
		{name: "no cell object", cr: &Controller{}, want: false},
		{
			name: "cell disabled",
			cr:   &Controller{Spec: ControllerSpec{Cell: &CellSpec{Enabled: false}}},
			want: false,
		},
		{
			name: "cell enabled",
			cr:   &Controller{Spec: ControllerSpec{Cell: &CellSpec{Enabled: true}}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCell(tc.cr); got != tc.want {
				t.Fatalf("IsCell = %v, want %v", got, tc.want)
			}
		})
	}
}
