package v1alpha1

import "testing"

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
