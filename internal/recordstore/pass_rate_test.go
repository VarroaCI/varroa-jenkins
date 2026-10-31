package recordstore

import "testing"

func TestCountsTowardPassRate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		result string
		want   bool
	}{
		{"SUCCESS", true},
		{"FAILURE", true},
		{"UNSTABLE", true},
		{"ABORTED", true},
		{"INTERRUPTED", false},
		{"NOT_BUILT", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := CountsTowardPassRate(tc.result); got != tc.want {
			t.Errorf("CountsTowardPassRate(%q) = %v; want %v", tc.result, got, tc.want)
		}
	}
}
