package recordstore

import "testing"

// TestWaitDiffers asserts that only a change to a displayed value makes a
// blocked claim write a wait notification.
func TestWaitDiffers(t *testing.T) {
	t.Parallel()

	base := priorWait{exists: true, kind: capKindTenant, name: "builds", max: 2, running: 2, cell: "cell-a"}
	verdict := capVerdict{Blocked: true, Kind: capKindTenant, Name: "builds", Max: 2, Running: 2}
	cases := []struct {
		name  string
		prior priorWait
		v     capVerdict
		cell  string
		want  bool
	}{
		{"no prior row", priorWait{}, verdict, "cell-a", true},
		{"identical row", base, verdict, "cell-a", false},
		{"running changed", base, capVerdict{Blocked: true, Kind: capKindTenant, Name: "builds", Max: 2, Running: 1}, "cell-a", true},
		{"max changed", base, capVerdict{Blocked: true, Kind: capKindTenant, Name: "builds", Max: 3, Running: 2}, "cell-a", true},
		{"kind changed", base, capVerdict{Blocked: true, Kind: capKindCell, Name: "builds", Max: 2, Running: 2}, "cell-a", true},
		{"name changed", base, capVerdict{Blocked: true, Kind: capKindTenant, Name: "other", Max: 2, Running: 2}, "cell-a", true},
		{"cell changed", base, verdict, "cell-b", true},
	}
	for _, tc := range cases {
		if got := waitDiffers(tc.prior, tc.v, tc.cell); got != tc.want {
			t.Errorf("%s: waitDiffers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
