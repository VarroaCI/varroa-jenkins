package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestItemsApplyReportsManagedCacheSaveFailure makes managed-items.json a
// directory, so the items walk applies but the cache save fails. The section
// must report failure and must not record last-items.yaml.
func TestItemsApplyReportsManagedCacheSaveFailure(t *testing.T) {
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
		t.Fatalf("expected items apply to fail on a cache save failure, got %+v", res)
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
