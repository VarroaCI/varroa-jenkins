package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// syncJobItems declares new-job and sets the sync removal strategy, so an
// undeclared managed item is a removal candidate.
const syncJobItems = "removeStrategy:\n  items: \"sync\"\n" + newJobItems

// deferralServer answers the items walk for one managed item, old-job. The
// building flag controls whether Jenkins reports a running build for it.
// deletes counts the delete requests the mite sends for it.
func deferralServer(t *testing.T, home string, building *atomic.Bool, deletes *atomic.Int32) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/job/old-job/api/json":
			w.Header().Set("Content-Type", "application/json")
			if building.Load() {
				fmt.Fprint(w, `{"color":"blue_anime","inQueue":false,"lastBuild":{"building":true},"jobs":[]}`)
				return
			}
			fmt.Fprint(w, `{"color":"blue","inQueue":false,"lastBuild":{"building":false},"jobs":[]}`)
		case r.URL.Path == "/job/old-job/doDelete":
			deletes.Add(1)
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/createItem"):
			mu.Lock()
			defer mu.Unlock()
			dir := filepath.Join(home, "jobs", "new-job")
			if err := os.MkdirAll(dir, 0755); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<flow-definition/>"), 0644); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeferredItemDeletionKeepsTheItemsSectionOutOfTheMarker proves a
// deferred deletion keeps the items section out of the marker, so each push
// walks the items again until the build ends and the item is deleted.
func TestDeferredItemDeletionKeepsTheItemsSectionOutOfTheMarker(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	var building atomic.Bool
	building.Store(true)
	var deletes atomic.Int32
	srv := deferralServer(t, home, &building, &deletes)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	if err := os.WriteFile(agent.managedItemsPath(), []byte(`{"old-job":"h"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		ItemsYaml:           syncJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if !first.ItemsSuccess || len(first.DeferredItemDeletions) != 1 {
		t.Fatalf("first apply: expected one deferred deletion and success, got %+v", first)
	}
	if _, ok := agent.readMarker().sections["items"]; ok {
		t.Fatal("marker records the items section while a deletion is deferred")
	}

	second := agent.handleDesiredState(context.Background(), cmd)
	if len(second.DeferredItemDeletions) != 1 {
		t.Fatalf("second push with the same hash: expected the walk to report the deferral again, got %+v", second)
	}

	building.Store(false)
	third := agent.handleDesiredState(context.Background(), cmd)
	if !third.ItemsSuccess || len(third.DeferredItemDeletions) != 0 {
		t.Fatalf("third push: expected no deferral once idle, got %+v", third)
	}
	if got := deletes.Load(); got != 1 {
		t.Fatalf("delete requests = %d, want 1", got)
	}
	if _, ok := agent.readMarker().sections["items"]; !ok {
		t.Fatal("marker lacks the items section after the deletion settled")
	}
}

// TestConvergedFastPathNeedsTheItemsSection proves the composite fast path
// does not skip a push whose marker lacks the items section, even when the
// desired-state hash and the boot session match.
func TestConvergedFastPathNeedsTheItemsSection(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	agent.writeMarker(appliedMarker{
		composite: "hash-1",
		session:   "sess-1",
		sections:  map[string]string{"config": "unrelated"},
	})

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		ItemsYaml:           newJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess {
		t.Fatalf("expected items apply to succeed, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "new-job", "config.xml")); err != nil {
		t.Fatalf("expected the items walk to create new-job, the fast path skipped it: %v", err)
	}
}
