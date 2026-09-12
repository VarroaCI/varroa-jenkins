package rehydrate

import (
	"reflect"
	"testing"
)

// TestPendingJobPaths_DeferredJob covers task 5.7a: a deferred job path
// alone is reported.
func TestPendingJobPaths_DeferredJob(t *testing.T) {
	got := PendingJobPaths([]string{"team/job-a"}, nil)
	want := []string{"team/job-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestPendingJobPaths_UncoveredJob covers task 5.7a: a job the round's
// selection has not covered yet is reported alongside a deferred one,
// deduplicated and sorted.
func TestPendingJobPaths_UncoveredJob(t *testing.T) {
	got := PendingJobPaths([]string{"team/job-b"}, []string{"team/job-a", "team/job-b"})
	want := []string{"team/job-a", "team/job-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
