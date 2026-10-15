package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// TestFetchTenantJobRecords_EnumerateThenPage covers task 5.8: a tenant
// with 70 jobs is fetched with one enumeration request (empty JobPaths)
// and three paging requests of at most 32 job paths each.
func TestFetchTenantJobRecords_EnumerateThenPage(t *testing.T) {
	total := 70
	allPaths := make([]string, total)
	for i := range allPaths {
		allPaths[i] = fmt.Sprintf("team/job-%03d", i)
	}

	var calls []cells.RecordsRequest
	fn := cells.RecordsFunc(func(_ context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
		calls = append(calls, req)
		if len(req.JobPaths) == 0 {
			// Enumeration: answer every job path, with no records.
			jobs := make([]cells.JobRecords, total)
			for i, p := range allPaths {
				jobs[i] = cells.JobRecords{JobPath: p, NextBuildNumber: 1}
			}
			return cells.RecordsResult{Jobs: jobs}, nil
		}
		// A page: answer only the requested paths.
		jobs := make([]cells.JobRecords, len(req.JobPaths))
		for i, p := range req.JobPaths {
			jobs[i] = cells.JobRecords{JobPath: p, NextBuildNumber: 1}
		}
		return cells.RecordsResult{Jobs: jobs}, nil
	})

	got, err := fetchTenantJobRecords(context.Background(), fn, "core", "team-a", 10)
	if err != nil {
		t.Fatalf("fetchTenantJobRecords: %v", err)
	}
	if len(got) != total {
		t.Fatalf("expected %d job records, got %d", total, len(got))
	}

	var enumerations, pages int
	pagedCount := 0
	for _, c := range calls {
		if len(c.JobPaths) == 0 {
			enumerations++
			continue
		}
		pages++
		if len(c.JobPaths) > rehydrationPageSize {
			t.Fatalf("expected at most %d paths per page, got %d", rehydrationPageSize, len(c.JobPaths))
		}
		pagedCount += len(c.JobPaths)
	}
	if enumerations != 1 {
		t.Fatalf("expected exactly one enumeration request, got %d", enumerations)
	}
	if pages != 3 {
		t.Fatalf("expected exactly three paging requests for 70 jobs, got %d", pages)
	}
	if pagedCount != total {
		t.Fatalf("expected every job path paged exactly once, got %d", pagedCount)
	}
}

// TestFetchTenantJobRecords_EmptyEnumeration covers the empty-selection
// input to task 6.0a: an enumeration with no job path sends no paging
// request and answers no error.
func TestFetchTenantJobRecords_EmptyEnumeration(t *testing.T) {
	fn := cells.RecordsFunc(func(_ context.Context, _ cells.RecordsRequest) (cells.RecordsResult, error) {
		return cells.RecordsResult{}, nil
	})
	got, err := fetchTenantJobRecords(context.Background(), fn, "core", "team-a", 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no job records, got %d", len(got))
	}
}
