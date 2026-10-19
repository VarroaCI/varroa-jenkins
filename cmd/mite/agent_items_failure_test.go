package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestHandleDesiredStateReportsItemsFailedPath checks that a refused item
// create names its item path in DesiredStateResult.ItemsFailedPath.
func TestHandleDesiredStateReportsItemsFailedPath(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/createItem"):
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "<html><head><title>Jenkins</title></head><body>Logging ID=abcdef12-3456</body></html>")
		case strings.HasSuffix(r.URL.Path, "/config.xml") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-items-fail",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "items:\n  - kind: folder\n    name: repos\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if res.ItemsSuccess {
		t.Fatalf("items apply against a refusing Jenkins must fail, got %+v", res)
	}
	if res.ItemsFailedPath != "repos" {
		t.Fatalf("ItemsFailedPath = %q, want repos", res.ItemsFailedPath)
	}
	if !strings.Contains(res.ItemsError, "create repos:") {
		t.Fatalf("ItemsError = %q, want it to name the create step", res.ItemsError)
	}
}

// TestHandleDesiredStateTransientCheckLeavesPathEmpty checks that a failed
// check of existence, a transient Jenkins fault, raises no tenant warning.
func TestHandleDesiredStateTransientCheckLeavesPathEmpty(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/config.xml") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-items-transient",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "items:\n  - kind: folder\n    name: repos\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if res.ItemsSuccess {
		t.Fatalf("items apply against a 503 must fail, got %+v", res)
	}
	if res.ItemsFailedPath != "" {
		t.Fatalf("ItemsFailedPath = %q, want empty for a transient check failure", res.ItemsFailedPath)
	}
	if !strings.Contains(res.ItemsError, "check existence of repos:") {
		t.Fatalf("ItemsError = %q, want it to name the check step", res.ItemsError)
	}
}
