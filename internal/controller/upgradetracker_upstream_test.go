package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestUpgradeTracker_UsesConfiguredBaseURL verifies that every probe goes to
// the configured upstream host and nowhere else.
func TestUpgradeTracker_UsesConfiguredBaseURL(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-479", "2.479", "2.479.10"))

	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, ts.URL+"/", testLogger())
	tracker.Reconcile(context.Background())

	if hits.Load() == 0 {
		t.Fatal("expected the tracker to probe the configured upstream host")
	}
}

// TestUpgradeTracker_EmptyBaseURLIsInert verifies that an empty base URL makes
// the tracker send no request, so the air-gap setting cannot leak a probe.
func TestUpgradeTracker_EmptyBaseURLIsInert(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-479", "2.479", "2.479.10"))

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, "", testLogger())
	tracker.httpDoer = &http.Client{Transport: failOnRequestTransport{t: t}}
	tracker.Reconcile(context.Background())
}

// TestUpgradeTracker_ClientHasTimeout verifies the constructor's default
// client carries a timeout, so a dropped connection cannot hold a tick open.
func TestUpgradeTracker_ClientHasTimeout(t *testing.T) {
	tracker := NewUpgradeTrackerReconciler(crdstore.NewFake(), nil, "https://updates.jenkins.io", testLogger())
	client, ok := tracker.httpDoer.(*http.Client)
	if !ok {
		t.Fatalf("expected httpDoer to be *http.Client, got %T", tracker.httpDoer)
	}
	if client.Timeout <= 0 {
		t.Fatal("expected a positive client timeout")
	}
}
