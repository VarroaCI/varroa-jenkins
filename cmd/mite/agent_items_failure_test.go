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

// TestHandleDesiredStateReportsItemFailures checks that a refused item
// create names its item path in DesiredStateResult.ItemFailures.
func TestHandleDesiredStateReportsItemFailures(t *testing.T) {
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
	if len(res.ItemFailures) != 1 {
		t.Fatalf("ItemFailures = %+v, want one entry", res.ItemFailures)
	}
	if res.ItemFailures[0].Path != "repos" {
		t.Fatalf("ItemFailures[0].Path = %q, want repos", res.ItemFailures[0].Path)
	}
	if !strings.Contains(res.ItemFailures[0].Error, "create repos:") {
		t.Fatalf("ItemFailures[0].Error = %q, want it to name the create step", res.ItemFailures[0].Error)
	}
	if !strings.Contains(res.ItemsError, "create repos:") {
		t.Fatalf("ItemsError = %q, want it to name the create step", res.ItemsError)
	}
}

// TestHandleDesiredStateReportsEveryTenantFailure checks that two refused
// items of two tenants both reach the result, so the operator can attribute
// each one.
func TestHandleDesiredStateReportsEveryTenantFailure(t *testing.T) {
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
		DesiredStateHash:    "hash-items-two-tenants",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "items:\n  - kind: folder\n    name: payments\n  - kind: folder\n    name: search\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if len(res.ItemFailures) != 2 {
		t.Fatalf("ItemFailures = %+v, want two entries", res.ItemFailures)
	}
	if res.ItemFailures[0].Path != "payments" || res.ItemFailures[1].Path != "search" {
		t.Fatalf("ItemFailures paths = %q, %q, want payments and search",
			res.ItemFailures[0].Path, res.ItemFailures[1].Path)
	}
}

// TestHandleDesiredStateTransientCheckLeavesFailuresEmpty checks that a failed
// check of existence, a transient Jenkins fault, raises no tenant warning.
func TestHandleDesiredStateTransientCheckLeavesFailuresEmpty(t *testing.T) {
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
	if len(res.ItemFailures) != 0 {
		t.Fatalf("ItemFailures = %+v, want none for a transient check failure", res.ItemFailures)
	}
	if !strings.Contains(res.ItemsError, "check existence of repos:") {
		t.Fatalf("ItemsError = %q, want it to name the check step", res.ItemsError)
	}
}
