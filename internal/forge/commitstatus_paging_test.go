package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// statusPage writes one page of n pending statuses with distinct contexts.
func statusPage(w http.ResponseWriter, prefix string, n int) {
	var b strings.Builder
	b.WriteString(`{"statuses":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"context":"%s-%d","state":"pending"}`, prefix, i)
	}
	b.WriteString(`]}`)
	_, _ = w.Write([]byte(b.String()))
}

// TestGitHubCommitStatusesReadsEveryPage checks that a full first page leads
// to a second page, so a matching status past the first page is not missed.
func TestGitHubCommitStatusesReadsEveryPage(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/" + testSHA + "/status": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				statusPage(w, "b", 1)
				return
			}
			statusPage(w, "a", statusPageSize)
		},
	})
	got, err := c.CommitStatuses(context.Background(), testRepo, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != statusPageSize+1 || got[len(got)-1].Context != "b-0" {
		t.Fatalf("got %d statuses, last %q", len(got), got[len(got)-1].Context)
	}
}

// TestGiteaCommitStatusesIgnoredPageParameter checks a forge that answers the
// same page for every page number. The repeated contexts are dropped, and the
// listing ends with the contexts that it holds.
func TestGiteaCommitStatusesIgnoredPageParameter(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/" + testSHA + "/status": func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString(`{"total_count":60,"statuses":[`)
			for i := 0; i < statusPageSize; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"context":"c-%d","status":"pending"}`, i)
			}
			b.WriteString(`]}`)
			_, _ = w.Write([]byte(b.String()))
		},
	})
	// The forge states 60 contexts and pages none of them, so the listing
	// is open. It must not answer a partial list as complete.
	_, err := c.CommitStatuses(context.Background(), testRepo, testSHA)
	if !errors.Is(err, ErrListingTruncated) {
		t.Fatalf("err = %v, want ErrListingTruncated", err)
	}
}

// TestGiteaCommitStatusesTruncatesAtPageBound checks that a listing past the
// page bound answers ErrListingTruncated, not a silent partial list.
func TestGiteaCommitStatusesTruncatesAtPageBound(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/" + testSHA + "/status": func(w http.ResponseWriter, r *http.Request) {
			statusPage(w, "p"+r.URL.Query().Get("page"), statusPageSize)
		},
	})
	_, err := c.CommitStatuses(context.Background(), testRepo, testSHA)
	if !errors.Is(err, ErrListingTruncated) {
		t.Fatalf("err = %v, want ErrListingTruncated", err)
	}
}
