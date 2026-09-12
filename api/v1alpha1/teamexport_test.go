package v1alpha1

import (
	"reflect"
	"testing"
)

// evalTeamExportExactlyOne mirrors:
//
//	(has(self.job)?1:0)+(has(self.locks)?1:0)==1
func evalTeamExportExactlyOne(jobSet, locksSet bool) bool {
	count := 0
	if jobSet {
		count++
	}
	if locksSet {
		count++
	}
	return count == 1
}

// evalTeamExportJobNonEmpty mirrors:
//
//	!has(self.job) || size(self.job) > 0
func evalTeamExportJobNonEmpty(jobSet bool, job string) bool {
	return !jobSet || len(job) > 0
}

// evalTeamExportJobNoLeadingSlash mirrors:
//
//	!has(self.job) || !self.job.startsWith('/')
func evalTeamExportJobNoLeadingSlash(jobSet bool, job string) bool {
	return !jobSet || len(job) == 0 || job[0] != '/'
}

// evalTeamExportLocksNonEmpty mirrors:
//
//	!has(self.locks) || self.locks.all(l, size(l) > 0)
func evalTeamExportLocksNonEmpty(locksSet bool, locks []string) bool {
	if !locksSet {
		return true
	}
	for _, l := range locks {
		if len(l) == 0 {
			return false
		}
	}
	return true
}

// evalTeamExportCallersNonEmpty mirrors:
//
//	self.callers.all(c, size(c) > 0)
func evalTeamExportCallersNonEmpty(callers []string) bool {
	for _, c := range callers {
		if len(c) == 0 {
			return false
		}
	}
	return true
}

func TestTeamExportCEL(t *testing.T) {
	tests := []struct {
		name         string
		jobSet       bool
		job          string
		locksSet     bool
		locks        []string
		callers      []string
		wantAccepted bool
	}{
		{
			name:         "a job entry with one caller is accepted",
			jobSet:       true,
			job:          "release/*",
			callers:      []string{"payments"},
			wantAccepted: true,
		},
		{
			name:         "a locks entry with one caller is accepted",
			locksSet:     true,
			locks:        []string{"deploy-prod"},
			callers:      []string{"alpha"},
			wantAccepted: true,
		},
		{
			name:         "neither job nor locks is refused",
			callers:      []string{"payments"},
			wantAccepted: false,
		},
		{
			name:         "both job and locks is refused",
			jobSet:       true,
			job:          "release/*",
			locksSet:     true,
			locks:        []string{"deploy-prod"},
			callers:      []string{"payments"},
			wantAccepted: false,
		},
		{
			name:         "an empty job is refused",
			jobSet:       true,
			job:          "",
			callers:      []string{"payments"},
			wantAccepted: false,
		},
		{
			name:         "a job with a leading slash is refused",
			jobSet:       true,
			job:          "/release",
			callers:      []string{"payments"},
			wantAccepted: false,
		},
		{
			name:         "an empty locks entry is refused",
			locksSet:     true,
			locks:        []string{"deploy-prod", ""},
			callers:      []string{"alpha"},
			wantAccepted: false,
		},
		{
			name:         "an empty callers entry is refused",
			jobSet:       true,
			job:          "release/*",
			callers:      []string{"payments", ""},
			wantAccepted: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accepted := evalTeamExportExactlyOne(tt.jobSet, tt.locksSet) &&
				evalTeamExportJobNonEmpty(tt.jobSet, tt.job) &&
				evalTeamExportJobNoLeadingSlash(tt.jobSet, tt.job) &&
				evalTeamExportLocksNonEmpty(tt.locksSet, tt.locks) &&
				evalTeamExportCallersNonEmpty(tt.callers)
			if accepted != tt.wantAccepted {
				t.Errorf("accepted = %v, want %v", accepted, tt.wantAccepted)
			}
		})
	}
}

// TestTeamExportDeepCopy asserts the hand-added DeepCopy round trips every
// field, including the two slices, and that the copy shares no backing
// array with the original.
func TestTeamExportDeepCopy(t *testing.T) {
	original := &TeamPipelinesSpec{
		Profile: "lts",
		Exports: []TeamExport{
			{Job: "release/*", Callers: []string{"payments", "search"}},
			{Locks: []string{"deploy-prod", "staging/*"}, Callers: []string{"payments"}},
		},
	}

	got := original.DeepCopy()
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("DeepCopy() = %+v, want %+v", got, original)
	}

	// Mutating the copy's slices must not affect the original.
	got.Exports[0].Callers[0] = "mutated"
	got.Exports[1].Locks[0] = "mutated"
	if original.Exports[0].Callers[0] == "mutated" {
		t.Error("DeepCopy shares Callers backing array with the original")
	}
	if original.Exports[1].Locks[0] == "mutated" {
		t.Error("DeepCopy shares Locks backing array with the original")
	}

	// A nil Exports list round trips as nil, not an empty slice.
	nilExports := &TeamPipelinesSpec{Profile: "lts"}
	if got := nilExports.DeepCopy(); got.Exports != nil {
		t.Errorf("DeepCopy() with no Exports = %+v, want nil Exports", got.Exports)
	}
}
