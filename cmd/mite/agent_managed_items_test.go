package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestItemsApplyReportsManagedCacheFailure makes managed-items.json a
// directory, so the cache read fails. The section must report failure and must
// not record last-items.yaml.
func TestItemsApplyReportsManagedCacheFailure(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	if err := os.MkdirAll(filepath.Join(agent.miteDir, "managed-items.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		ItemsYaml:           newJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)
	if res.ItemsSuccess {
		t.Fatalf("expected items apply to fail on a cache failure, got %+v", res)
	}
	if !strings.Contains(res.ItemsError, "managed-items.json") {
		t.Fatalf("ItemsError = %q, want it to name managed-items.json", res.ItemsError)
	}
	if len(res.ItemFailures) != 0 {
		t.Fatalf("ItemFailures = %v, want none", res.ItemFailures)
	}
	if _, err := os.Stat(filepath.Join(agent.miteDir, "last-items.yaml")); !os.IsNotExist(err) {
		t.Fatalf("last-items.yaml must not be written on a save failure, stat err = %v", err)
	}
}

// TestItemsApplyReportsCorruptManagedCache seeds a managed-items cache that is
// not JSON. The section must fail, name the cache file, apply no item, and keep
// the corrupt file unchanged.
func TestItemsApplyReportsCorruptManagedCache(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cache := filepath.Join(agent.miteDir, "managed-items.json")
	if err := os.WriteFile(cache, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		ItemsYaml:           newJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)
	if res.ItemsSuccess {
		t.Fatalf("expected items apply to fail on a corrupt cache, got %+v", res)
	}
	if !strings.Contains(res.ItemsError, "managed-items.json") {
		t.Fatalf("ItemsError = %q, want it to name managed-items.json", res.ItemsError)
	}
	if len(res.ItemFailures) != 0 {
		t.Fatalf("ItemFailures = %v, want none", res.ItemFailures)
	}
	data, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{not json" {
		t.Fatalf("corrupt cache was rewritten: %q", data)
	}
	if _, err := os.Stat(filepath.Join(agent.miteDir, "last-items.yaml")); !os.IsNotExist(err) {
		t.Fatalf("last-items.yaml must not be written on a cache failure, stat err = %v", err)
	}
}

// TestItemsApplyPersistsManagedHashesUnderMiteDir proves the items engine keeps
// its managed-items cache under the agent's miteDir, not under the Jenkins home.
func TestItemsApplyPersistsManagedHashesUnderMiteDir(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

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

	data, err := os.ReadFile(filepath.Join(agent.miteDir, "managed-items.json"))
	if err != nil {
		t.Fatalf("expected managed-items cache under miteDir: %v", err)
	}
	var hashes map[string]string
	if err := json.Unmarshal(data, &hashes); err != nil {
		t.Fatalf("unmarshal managed-items cache: %v", err)
	}
	if hashes["new-job"] == "" {
		t.Fatalf("expected a non-empty hash for new-job, got %v", hashes)
	}
}

// TestManagedItemsPathIsUnderMiteDir pins the managed-items path to miteDir.
func TestManagedItemsPathIsUnderMiteDir(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	if got, want := agent.managedItemsPath(), "/var/jenkins_home/varroa-mite/managed-items.json"; got != want {
		t.Fatalf("default managedItemsPath = %q, want %q", got, want)
	}

	d := t.TempDir()
	agent.miteDir = d
	if got, want := agent.managedItemsPath(), filepath.Join(d, "managed-items.json"); got != want {
		t.Fatalf("managedItemsPath = %q, want %q", got, want)
	}
}

// runDeleteItem runs one delete_item command against a Jenkins stub that answers
// the doDelete call with deleteStatus. It seeds managed-items.json with cache
// and returns the command result.
func runDeleteItem(t *testing.T, deleteStatus int, cache string) (*mitev1.CommandResult, *Agent) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/crumbIssuer/api/json":
			w.WriteHeader(http.StatusNotFound)
		case "/job/old-job/doDelete":
			w.WriteHeader(deleteStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()
	setTestStream(agent, stream)
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	if cache != "" {
		if err := os.WriteFile(agent.managedItemsPath(), []byte(cache), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: &mitev1.ImperativeCommand{
			CommandId: "delete-old-job",
			Type:      mitev1.CommandTypeDeleteItem,
			Target:    "old-job",
		},
		ctx: context.Background(),
	})
	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	return result, agent
}

// TestDeleteItemCommandForgetsManagedItem checks that a successful approved
// delete removes the item from the managed-items record.
func TestDeleteItemCommandForgetsManagedItem(t *testing.T) {
	result, agent := runDeleteItem(t, http.StatusOK, `{"old-job":"h","keep":"h"}`)
	if !result.Success {
		t.Fatalf("Success = false, error %q", result.Error)
	}
	assertManagedCache(t, agent, `{"keep":"h"}`)
}

// TestDeleteItemCommandNotFoundForgetsManagedItem checks that a 404 counts as a
// completed delete, so the record drops the item.
func TestDeleteItemCommandNotFoundForgetsManagedItem(t *testing.T) {
	result, agent := runDeleteItem(t, http.StatusNotFound, `{"old-job":"h","keep":"h"}`)
	if !result.Success {
		t.Fatalf("Success = false, error %q", result.Error)
	}
	assertManagedCache(t, agent, `{"keep":"h"}`)
}

// TestDeleteItemCommandServerErrorKeepsManagedItem checks that a failed delete
// leaves the item, and its record entry, unchanged.
func TestDeleteItemCommandServerErrorKeepsManagedItem(t *testing.T) {
	const cache = `{"old-job":"h","keep":"h"}`
	result, agent := runDeleteItem(t, http.StatusInternalServerError, cache)
	if result.Success {
		t.Fatal("Success = true, want false on a 500 delete")
	}
	assertManagedCache(t, agent, cache)
}

// TestDeleteItemCommandReportsRecordFailure checks that a delete which succeeds
// in Jenkins but cannot update the record reports failure and names the cache.
func TestDeleteItemCommandReportsRecordFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/job/old-job/doDelete" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()
	setTestStream(agent, stream)
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	if err := os.MkdirAll(agent.managedItemsPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: &mitev1.ImperativeCommand{
			CommandId: "delete-old-job",
			Type:      mitev1.CommandTypeDeleteItem,
			Target:    "old-job",
		},
		ctx: context.Background(),
	})
	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success {
		t.Fatal("Success = true, want false when the record cannot be updated")
	}
	if !strings.Contains(result.Error, "managed-items.json") {
		t.Fatalf("Error = %q, want it to name managed-items.json", result.Error)
	}
}

// assertManagedCache compares the managed-items record on disk with want as
// JSON objects.
func assertManagedCache(t *testing.T, agent *Agent, want string) {
	t.Helper()
	data, err := os.ReadFile(agent.managedItemsPath())
	if err != nil {
		t.Fatal(err)
	}
	var got, exp map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("cache %q is not JSON: %v", data, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(exp) {
		t.Fatalf("cache = %v, want %v", got, exp)
	}
	for k, v := range exp {
		if got[k] != v {
			t.Fatalf("cache = %v, want %v", got, exp)
		}
	}
}
