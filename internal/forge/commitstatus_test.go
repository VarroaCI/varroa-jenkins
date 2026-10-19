package forge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestGiteaCommitStatuses checks that the state is read from the status key
// and that the combined endpoint is called with the limit.
func TestGiteaCommitStatuses(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/" + testSHA + "/status": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("limit"); got != "50" {
				t.Errorf("limit = %q, want 50", got)
			}
			jsonReply(`{"statuses":[`+
				`{"context":"ci/a","status":"pending","target_url":"https://j/1/"},`+
				`{"context":"ci/b","state":"success","target_url":"https://j/2/"}]}`)(w, r)
		},
	})
	got, err := c.CommitStatuses(context.Background(), testRepo, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].State != "pending" || got[0].Context != "ci/a" || got[1].State != "success" {
		t.Fatalf("statuses = %+v", got)
	}
}

// TestGiteaSetCommitStatus checks the method, the path and the JSON body.
func TestGiteaSetCommitStatus(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/statuses/" + testSHA: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			for _, want := range []string{`"state":"error"`, `"context":"ci/a"`, `"target_url":"https://j/1/"`, `"description":"d"`} {
				if !strings.Contains(string(body), want) {
					t.Errorf("body %s lacks %s", body, want)
				}
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		},
	})
	err := c.SetCommitStatus(context.Background(), testRepo, testSHA, CommitStatus{
		Context: "ci/a", State: StateError, TargetURL: "https://j/1/", Description: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGitHubCommitStatuses checks the state key and the per_page query.
func TestGitHubCommitStatuses(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/" + testSHA + "/status": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("per_page"); got != "50" {
				t.Errorf("per_page = %q, want 50", got)
			}
			jsonReply(`{"state":"pending","statuses":[{"context":"ci/a","state":"pending","target_url":"https://j/1/"}]}`)(w, r)
		},
	})
	got, err := c.CommitStatuses(context.Background(), testRepo, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "pending" || got[0].TargetURL != "https://j/1/" {
		t.Fatalf("statuses = %+v", got)
	}
}

// TestGitHubSetCommitStatus checks the method, the path and the JSON body.
func TestGitHubSetCommitStatus(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/statuses/" + testSHA: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"state":"error"`) || !strings.Contains(string(body), `"context":"ci/a"`) {
				t.Errorf("body %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		},
	})
	err := c.SetCommitStatus(context.Background(), testRepo, testSHA, CommitStatus{
		Context: "ci/a", State: StateError, TargetURL: "https://j/1/", Description: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
}
