// Package jenkinsver provides helpers for parsing and comparing Jenkins version
// strings from container tags (e.g. "2.479.3-jdk17").
package jenkinsver

import (
	"strconv"
	"strings"
)

// Core strips the tag suffix (everything after the first '-') and returns the
// numeric segments of the core version. Returns nil, false when the tag is
// empty or any segment is non-numeric.
func Core(tag string) ([]int, bool) {
	core, _, _ := strings.Cut(tag, "-")
	core = strings.TrimSpace(core)
	if core == "" {
		return nil, false
	}
	segs := strings.Split(core, ".")
	out := make([]int, 0, len(segs))
	for _, s := range segs {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// Compare performs a segment-wise numeric comparison of two core-version
// slices. Missing trailing segments are treated as 0.
func Compare(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var va, vb int
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// AtLeast returns true when tag's core version is >= floor. ok is false when
// either version string is unparseable.
func AtLeast(tag, floor string) (atLeast bool, ok bool) {
	coreTag, ok := Core(tag)
	if !ok {
		return false, false
	}
	coreFloor, ok := Core(floor)
	if !ok {
		return false, false
	}
	return Compare(coreTag, coreFloor) >= 0, true
}
