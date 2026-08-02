package jenkinsver

import (
	"reflect"
	"testing"
)

func TestCore(t *testing.T) {
	tests := []struct {
		tag string
		ver []int
		ok  bool
	}{
		{"2.479.3-jdk17", []int{2, 479, 3}, true},
		{"2.570", []int{2, 570}, true},
		{"", nil, false},
		{"lts", nil, false},
		{"latest", nil, false},
		{"fancy-custom-build", nil, false},
	}
	for _, tc := range tests {
		got, ok := Core(tc.tag)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.ver) {
			t.Errorf("Core(%q) = (%v, %v), want (%v, %v)", tc.tag, got, ok, tc.ver, tc.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b []int
		want int
	}{
		{[]int{2, 479}, []int{2, 479, 0}, 0},
		{[]int{2, 479}, []int{2, 479, 1}, -1},
		{[]int{2, 479, 3}, []int{2, 479, 3}, 0},
		{[]int{2, 570}, []int{2, 479}, 1},
		{[]int{2, 479}, []int{2, 570}, -1},
		{[]int{2, 479, 3}, []int{2, 479, 2}, 1},
	}
	for _, tc := range tests {
		got := Compare(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		tag, floor  string
		atLeast, ok bool
	}{
		{"2.560", "2.570", false, true},
		{"2.579.1-jdk17", "2.570", true, true},
		{"lts", "2.570", false, false},
		{"2.570", "2.570", true, true},
		{"2.571", "2.570", true, true},
	}
	for _, tc := range tests {
		atLeast, ok := AtLeast(tc.tag, tc.floor)
		if atLeast != tc.atLeast || ok != tc.ok {
			t.Errorf("AtLeast(%q, %q) = (%v, %v), want (%v, %v)",
				tc.tag, tc.floor, atLeast, ok, tc.atLeast, tc.ok)
		}
	}
}
