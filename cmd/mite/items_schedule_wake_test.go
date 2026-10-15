package main

import (
	"context"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestItemsApplyWakesSchedulePoller proves an items apply that writes the
// section wakes the schedule poller, so a job it creates reaches the
// inventory without waiting for the poll interval.
func TestItemsApplyWakesSchedulePoller(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	agent.jobScheduleExportOpen.Store(true)

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
	select {
	case <-agent.jobSchedulePollNow:
	default:
		t.Fatal("expected the items apply to wake the schedule poller")
	}
}

// TestConvergedItemsSectionDoesNotWakeSchedulePoller proves a push whose
// items section already converged does not wake the poller. The whole-state
// hash differs, so the whole-state early return does not hide the section path.
func TestConvergedItemsSectionDoesNotWakeSchedulePoller(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, nil, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	agent.jobScheduleExportOpen.Store(true)
	agent.writeMarker(appliedMarker{
		composite: "hash-old",
		session:   "sess-1",
		sections:  map[string]string{"items": sha256hex([]byte(newJobItems))},
	})

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-new",
		ItemsYaml:           newJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess {
		t.Fatalf("expected converged items to report success, got %+v", res)
	}
	select {
	case <-agent.jobSchedulePollNow:
		t.Fatal("a converged items section must not wake the schedule poller")
	default:
	}
}
