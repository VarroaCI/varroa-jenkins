package api

import "sort"

// dedupeSort sorts a string slice ascending and removes duplicates.
// Returns a non-nil empty slice for nil/empty input (never serializes to JSON null).
func dedupeSort(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	// Sort first, then compact duplicates.
	sort.Strings(in)
	out := make([]string, 0, len(in))
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}

// intersect returns the sorted, deduped intersection of a and b.
// Returns a non-nil empty slice if either input is empty.
func intersect(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return []string{}
	}
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	out := make([]string, 0)
	for _, s := range a {
		if _, ok := set[s]; ok {
			out = append(out, s)
		}
	}
	return dedupeSort(out)
}

// sliceContains reports whether a string slice contains a given string.
func sliceContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
