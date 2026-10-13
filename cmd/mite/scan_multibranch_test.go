package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// scanCommand returns one SCAN_MULTIBRANCH imperative for the job.
func scanCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "scan-multibranch-s1",
		Type:      mitev1.CommandTypeScanMultibranch,
		ScanMultibranch: &mitev1.ScanMultibranchPayload{
			ScanId:     "s1",
			JobPath:    "team-a/multi",
			ActingUser: &mitev1.ActingUser{Name: "ana"},
		},
	}
}

// TestRunCommandScanMultibranch asserts an accepted scan posts the scan
// identifier and the job path to the plugin's scan route, reports 202, and
// opens the fast-poll window.
func TestRunCommandScanMultibranch(t *testing.T) {
	var posted map[string]any
	var scanPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		scanPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &posted)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	setTestStream(agent, stream)
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{imperativeCmd: scanCommand(), ctx: context.Background()})

	if scanPath != "/varroa-run/scan" {
		t.Fatalf("path = %q, want /varroa-run/scan", scanPath)
	}
	if posted["scanId"] != "s1" || posted["jobPath"] != "team-a/multi" {
		t.Fatalf("body = %+v", posted)
	}
	result := captured.Load()
	if result == nil || !result.Success || result.HttpStatus != http.StatusAccepted {
		t.Fatalf("result = %+v, want success with status 202", result)
	}
	if result.Actor != "ana" || result.CommandType != mitev1.CommandTypeScanMultibranch {
		t.Fatalf("result actor = %q, type = %v", result.Actor, result.CommandType)
	}
	if !agent.branchScanFast(time.Now()) {
		t.Fatal("an accepted scan did not open the fast-poll window")
	}
}

// TestRunCommandScanMultibranchRefusalOpensNoWindow asserts a refused scan
// reports the plugin's status and leaves the poller at its usual interval.
func TestRunCommandScanMultibranchRefusalOpensNoWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, "Forbidden: the acting user may not scan this job", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	setTestStream(agent, stream)
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{imperativeCmd: scanCommand(), ctx: context.Background()})

	result := captured.Load()
	if result == nil || result.Success || result.HttpStatus != http.StatusForbidden {
		t.Fatalf("result = %+v, want a refusal with status 403", result)
	}
	if agent.branchScanFast(time.Now()) {
		t.Fatal("a refused scan opened the fast-poll window")
	}
}

// TestBranchScanFastWindowEndsAfterFifteenMinutes asserts the window covers
// the first fifteen minutes after an accepted scan and no later time.
func TestBranchScanFastWindowEndsAfterFifteenMinutes(t *testing.T) {
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns"})
	start := time.Now()
	agent.openBranchScanWindow(start)

	if !agent.branchScanFast(start.Add(14 * time.Minute)) {
		t.Fatal("window closed before fifteen minutes")
	}
	if agent.branchScanFast(start.Add(16 * time.Minute)) {
		t.Fatal("window stayed open after fifteen minutes")
	}
}

// TestLastScanProtoCarriesTheScan asserts the plugin's last scan row maps
// onto the proto result field by field, and a nil row maps to nil.
func TestLastScanProtoCarriesTheScan(t *testing.T) {
	if got := lastScanProto(nil); got != nil {
		t.Fatalf("lastScanProto(nil) = %+v, want nil", got)
	}
	row := &jobBranchScanRow{
		ScanID: "s1", State: "completed", Actor: "ana",
		RequestedAt: "2026-10-12T10:00:00Z", EndedAt: "2026-10-12T10:02:00Z", Result: "SUCCESS",
		NewHeadRefs:      []jobBranchHeadRef{{Name: "feature", JobPath: "mb/feature"}},
		ChangedHeadRefs:  []jobBranchHeadRef{{Name: "main", JobPath: "mb/main"}},
		OrphanedHeadRefs: []jobBranchHeadRef{{Name: "old", JobPath: "mb/old"}},
	}
	got := lastScanProto(row)
	if got.ScanId != "s1" || got.State != "completed" || got.Actor != "ana" ||
		got.RequestedAt != "2026-10-12T10:00:00Z" || got.EndedAt != "2026-10-12T10:02:00Z" || got.Result != "SUCCESS" ||
		len(got.NewHeadRefs) != 1 || got.NewHeadRefs[0].Name != "feature" || got.NewHeadRefs[0].JobPath != "mb/feature" ||
		len(got.ChangedHeadRefs) != 1 || got.ChangedHeadRefs[0].Name != "main" ||
		len(got.OrphanedHeadRefs) != 1 || got.OrphanedHeadRefs[0].Name != "old" || got.OrphanedHeadRefs[0].JobPath != "mb/old" {
		t.Fatalf("lastScanProto = %+v", got)
	}
}
