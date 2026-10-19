package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeLedgerActionRunner records every call handleLedgerAction makes, so
// a test can assert the mapping without a full Reconciler (task 9.1a).
//
// hibernated and events model WakeHibernatedController's real
// compare-and-swap semantics (task 7.2): the production method wakes
// only a controller that is currently hibernated, and is a no-op
// otherwise. Setting hibernated before a call and reading events
// afterward lets a test assert both the wake-before-send ordering and
// the "no wake" outcome for a connected target, without a full
// Reconciler.
type fakeLedgerActionRunner struct {
	sent      []*mitev1.ImperativeCommand
	sentNS    string
	sentName  string
	sendErr   error
	wokeNS    string
	wokeName  string
	wokeCalls int

	hibernated bool
	events     []string

	releasedTeam      string
	releasedNamespace string
	releasedNames     []string
	releaseCalls      int

	instanceReleaseCalls int
	instanceTeam         string
	instanceNamespace    string
	instanceKeep         []string
	instanceBefore       time.Time
}

// ReleaseInstanceAgentPods records the call a release_instance_agents
// action makes.
func (f *fakeLedgerActionRunner) ReleaseInstanceAgentPods(_ context.Context, team, namespace string, keep []string, refreshedBefore time.Time) {
	f.instanceReleaseCalls++
	f.instanceTeam = team
	f.instanceNamespace = namespace
	f.instanceKeep = keep
	f.instanceBefore = refreshedBefore
}

// ReleaseRunAgentPods records the call a release_agents action makes.
func (f *fakeLedgerActionRunner) ReleaseRunAgentPods(_ context.Context, team, namespace string, names []string) {
	f.releaseCalls++
	f.releasedTeam = team
	f.releasedNamespace = namespace
	f.releasedNames = names
}

func (f *fakeLedgerActionRunner) SendImperativeCommand(_ context.Context, namespace, name string, cmd *mitev1.ImperativeCommand) error {
	f.sent = append(f.sent, cmd)
	f.sentNS = namespace
	f.sentName = name
	f.events = append(f.events, "send")
	return f.sendErr
}

func (f *fakeLedgerActionRunner) WakeHibernatedController(_ context.Context, namespace, name string) {
	f.wokeCalls++
	f.wokeNS = namespace
	f.wokeName = name
	if f.hibernated {
		f.events = append(f.events, "wake")
		f.hibernated = false
	}
}

func testLogger() *slog.Logger { return slog.Default() }

// TestHandleLedgerAction_CancelRun covers task 9.1a: cancel_run maps to
// CANCEL_BUILD, addressed from the action's own namespace/controller.
func TestHandleLedgerAction_CancelRun(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	bn := int64(7)
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionCancelRun, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-1", JobPath: "team/job-a", BuildNumber: &bn,
		Actor: recordstore.LedgerActionRecorder,
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	cmd := rec.sent[0]
	if cmd.Type != mitev1.CommandTypeCancelBuild {
		t.Fatalf("expected CANCEL_BUILD, got %v", cmd.Type)
	}
	if cmd.CancelBuild == nil || cmd.CancelBuild.RunId != "run-1" || cmd.CancelBuild.JobPath != "team/job-a" || cmd.CancelBuild.BuildNumber != 7 {
		t.Fatalf("unexpected CancelBuild payload: %+v", cmd.CancelBuild)
	}
	if cmd.CancelBuild.Actor != recordstore.LedgerActionRecorder {
		t.Fatalf("cancel actor = %q, want %q", cmd.CancelBuild.Actor, recordstore.LedgerActionRecorder)
	}
	if rec.sentNS != "cells-ns" || rec.sentName != "cell-a" {
		t.Fatalf("expected the command addressed to cells-ns/cell-a, got %s/%s", rec.sentNS, rec.sentName)
	}
}

// TestHandleLedgerAction_RetryRun covers task 9.1a: retry_run maps to
// TRIGGER_BUILD, carrying the action's intent key and parameters.
func TestHandleLedgerAction_RetryRun(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionRetryRun, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-2", IntentKey: "retry:run-2", JobPath: "team/job-a",
		Parameters: json.RawMessage(`{"BRANCH":"main"}`),
		Actor:      "varroa-recorder",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	cmd := rec.sent[0]
	if cmd.Type != mitev1.CommandTypeTriggerBuild {
		t.Fatalf("expected TRIGGER_BUILD, got %v", cmd.Type)
	}
	tb := cmd.TriggerBuild
	if tb == nil || tb.RunId != "run-2" || tb.IntentKey != "retry:run-2" || tb.JobPath != "team/job-a" || tb.Actor != "varroa-recorder" {
		t.Fatalf("unexpected TriggerBuild payload: %+v", tb)
	}
	if tb.Parameters["BRANCH"] != "main" {
		t.Fatalf("expected parameters decoded, got %+v", tb.Parameters)
	}
}

// TestHandleLedgerAction_TriggerRun covers task 7.1: trigger_run maps to
// TRIGGER_BUILD, carrying the action's run id, intent key, job path,
// parameters, and actor, addressed to the action's own namespace and
// controller.
func TestHandleLedgerAction_TriggerRun(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "trigger_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-3", IntentKey: "upstream:run-2:t1a2b3c4d5e6", JobPath: "team/job-a",
		Parameters: json.RawMessage(`{"BRANCH":"main"}`),
		Actor:      "svc-payments",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	cmd := rec.sent[0]
	if cmd.Type != mitev1.CommandTypeTriggerBuild {
		t.Fatalf("expected TRIGGER_BUILD, got %v", cmd.Type)
	}
	tb := cmd.TriggerBuild
	if tb == nil || tb.RunId != "run-3" || tb.IntentKey != "upstream:run-2:t1a2b3c4d5e6" || tb.JobPath != "team/job-a" || tb.Actor != "svc-payments" {
		t.Fatalf("unexpected TriggerBuild payload: %+v", tb)
	}
	if tb.Parameters["BRANCH"] != "main" {
		t.Fatalf("expected parameters decoded, got %+v", tb.Parameters)
	}
	if rec.sentNS != "cells-ns" || rec.sentName != "cell-a" {
		t.Fatalf("expected the command addressed to cells-ns/cell-a, got %s/%s", rec.sentNS, rec.sentName)
	}
}

// TestHandleLedgerAction_TriggerRun_OtherCluster covers task 7.1's
// acceptance that the wrong-cluster filter stays unchanged for the new
// kind: an action naming another cluster is ignored before the switch
// ever inspects the kind.
func TestHandleLedgerAction_TriggerRun_OtherCluster(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "trigger_run", Cluster: "hive-b",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-3", IntentKey: "upstream:run-2:t1a2b3c4d5e6", JobPath: "team/job-a",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 0 || rec.wokeCalls != 0 {
		t.Fatalf("expected no action for another cluster's trigger_run, got sent=%d woke=%d", len(rec.sent), rec.wokeCalls)
	}
}

// TestHandleLedgerAction_TriggerRun_WakesHibernatedTargetFirst covers
// task 7.2: a hibernated target is woken through the existing wake path
// before the imperative command is sent.
func TestHandleLedgerAction_TriggerRun_WakesHibernatedTargetFirst(t *testing.T) {
	rec := &fakeLedgerActionRunner{hibernated: true}
	action := recordstore.LedgerAction{
		Kind: "trigger_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-4", IntentKey: "upstream:run-3:t2b3c4d5e6f7", JobPath: "team/job-a",
		Actor: "svc-payments",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	want := []string{"wake", "send"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("expected the wake to precede the imperative send, got %v", rec.events)
	}
	if rec.wokeNS != "cells-ns" || rec.wokeName != "cell-a" {
		t.Fatalf("expected the wake addressed to cells-ns/cell-a, got %s/%s", rec.wokeNS, rec.wokeName)
	}
}

// TestHandleLedgerAction_TriggerRun_ConnectedTargetNotWoken covers task
// 7.2: a connected target is not woken; only the imperative command is
// sent.
func TestHandleLedgerAction_TriggerRun_ConnectedTargetNotWoken(t *testing.T) {
	rec := &fakeLedgerActionRunner{hibernated: false}
	action := recordstore.LedgerAction{
		Kind: "trigger_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-5", IntentKey: "upstream:run-4:t3c4d5e6f7a8", JobPath: "team/job-a",
		Actor: "svc-payments",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	want := []string{"send"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("expected no wake for a connected target, got %v", rec.events)
	}
}

// TestHandleLedgerAction_WakeCell covers task 9.1a: wake_cell calls the
// operator's own wake routine directly and publishes no message.
func TestHandleLedgerAction_WakeCell(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionWakeCell, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if rec.wokeCalls != 1 || rec.wokeNS != "cells-ns" || rec.wokeName != "cell-a" {
		t.Fatalf("expected WakeHibernatedController called once for cells-ns/cell-a, got calls=%d ns=%s name=%s",
			rec.wokeCalls, rec.wokeNS, rec.wokeName)
	}
	if len(rec.sent) != 0 {
		t.Fatalf("expected no imperative command for wake_cell, got %d", len(rec.sent))
	}
}

// TestHandleLedgerAction_UnknownKind covers task 9.1a: an unknown kind
// takes no action.
func TestHandleLedgerAction_UnknownKind(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{Kind: "delete_everything", Cluster: "core", Namespace: "cells-ns", Controller: "cell-a"}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 0 || rec.wokeCalls != 0 {
		t.Fatalf("expected no action for an unknown kind, got sent=%d woke=%d", len(rec.sent), rec.wokeCalls)
	}
}

// TestHandleLedgerAction_OtherCluster covers task 9.1a: an action naming
// another cluster is ignored.
func TestHandleLedgerAction_OtherCluster(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionWakeCell, Cluster: "hive-b",
		Namespace: "cells-ns", Controller: "cell-a",
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 0 || rec.wokeCalls != 0 {
		t.Fatalf("expected no action for another cluster's action, got sent=%d woke=%d", len(rec.sent), rec.wokeCalls)
	}
}

// TestHandleLedgerAction_ResumeRunCarriesItsDeadline pins the deadline
// design D12 names. Without it a command that reaches its target late
// restores a run the recorder has already marked, which a live run on
// core did: the mite wrote the files six minutes past the resume's own
// deadline and the build ended at once.
func TestHandleLedgerAction_ResumeRunCarriesItsDeadline(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "resume_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-9", JobPath: "team/job-a", Instance: "pod-live",
		Files: []recordstore.ResumeFile{{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 1737000000000}},
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	cmd := rec.sent[0]
	if cmd.Type != mitev1.CommandTypeResumeBuild {
		t.Fatalf("expected RESUME_BUILD, got %v", cmd.Type)
	}
	if cmd.DeadlineSec != resumeCommandDeadlineSec {
		t.Fatalf("DeadlineSec = %d, want %d", cmd.DeadlineSec, resumeCommandDeadlineSec)
	}
	rb := cmd.ResumeBuild
	if rb == nil || rb.Instance != "pod-live" || rb.RunId != "run-9" {
		t.Fatalf("unexpected ResumeBuild payload: %+v", rb)
	}
}

// TestHandleLedgerAction_ResumeRunIdentifiesItsTarget proves two resume
// attempts for one run carry different command identifiers.
//
// One run can hold several attempts inside its arm window. The gateway
// files one pending message for each command identifier on a
// controller, so a shared identifier would file one entry for both
// attempts. A late result from the first attempt would then acknowledge
// the second attempt's message, and the gateway would record that
// failure against the second target and strand a live resume.
func TestHandleLedgerAction_ResumeRunIdentifiesItsTarget(t *testing.T) {
	send := func(instance string) string {
		rec := &fakeLedgerActionRunner{}
		action := recordstore.LedgerAction{
			Kind: "resume_run", Cluster: "core",
			Namespace: "cells-ns", Controller: "cell-a",
			RunID: "run-9", JobPath: "team/job-a", Instance: instance,
			Files: []recordstore.ResumeFile{{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 1737000000000}},
		}
		data, _ := json.Marshal(action)
		handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)
		if len(rec.sent) != 1 {
			t.Fatalf("expected one imperative command, got %d", len(rec.sent))
		}
		return rec.sent[0].CommandId
	}

	first := send("pod-live")
	second := send("pod-live-2")
	if first == second {
		t.Fatalf("both attempts carry the command id %q, want one for each target", first)
	}
	if !strings.Contains(first, "run-9") || !strings.Contains(first, "pod-live") {
		t.Fatalf("command id = %q, want the run and the target instance", first)
	}
}

// TestHandleLedgerAction_ResumeRunCarriesTheAbsoluteExpiry proves the
// command carries the moment the recorder gives up, not only a
// duration. DeadlineSec bounds the work once the worker starts it, and
// that window opens at pickup, so it cannot end a command that waited
// in the queue.
func TestHandleLedgerAction_ResumeRunCarriesTheAbsoluteExpiry(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	deadline := time.Now().Add(6 * time.Minute).UTC()
	action := recordstore.LedgerAction{
		Kind: "resume_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-9", JobPath: "team/job-a", Instance: "pod-live",
		Files:    []recordstore.ResumeFile{{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 1737000000000}},
		Deadline: &deadline,
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	rb := rec.sent[0].ResumeBuild
	if rb == nil {
		t.Fatal("expected a ResumeBuild payload")
	}
	if rb.DeadlineUnix != deadline.Unix() {
		t.Fatalf("DeadlineUnix = %d, want %d", rb.DeadlineUnix, deadline.Unix())
	}
}

// TestHandleLedgerAction_ResumeRunWithoutADeadlineSendsZero proves an
// action that carries no deadline leaves the field empty, so the mite
// applies no expiry test.
func TestHandleLedgerAction_ResumeRunWithoutADeadlineSendsZero(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "resume_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-9", JobPath: "team/job-a", Instance: "pod-live",
		Files: []recordstore.ResumeFile{{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 1737000000000}},
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	if rb := rec.sent[0].ResumeBuild; rb == nil || rb.DeadlineUnix != 0 {
		t.Fatalf("DeadlineUnix = %+v, want 0", rb)
	}
}

// TestHandleLedgerAction_ResumeRunCarriesEachFileEpoch proves the
// operator carries every action entry's own epoch into the command. The
// run-state object key names the epoch, so a path alone names no object
// and the target could fetch nothing.
func TestHandleLedgerAction_ResumeRunCarriesEachFileEpoch(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "resume_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-9", JobPath: "team/job-a", Instance: "pod-live",
		Files: []recordstore.ResumeFile{
			{RelPath: "jobs/team/jobs/job-a/builds/5/program.dat", Epoch: 100},
			{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 200},
		},
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 {
		t.Fatalf("expected one imperative command, got %d", len(rec.sent))
	}
	rb := rec.sent[0].ResumeBuild
	if rb == nil || len(rb.StateFiles) != 2 {
		t.Fatalf("payload = %+v, want two state files", rb)
	}
	for i, want := range action.Files {
		got := rb.StateFiles[i]
		if got.Relpath != want.RelPath || got.PusherEpoch != want.Epoch {
			t.Fatalf("state_files[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestHandleLedgerAction_ResumeRunCarriesTheLogNextSeq proves the
// operator copies the action's log sequence into the command. The target
// holds no copy of the log, so the command is its only source.
func TestHandleLedgerAction_ResumeRunCarriesTheLogNextSeq(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: "resume_run", Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a",
		RunID: "run-9", JobPath: "team/job-a", Instance: "pod-live",
		Files:      []recordstore.ResumeFile{{RelPath: "jobs/team/jobs/job-a/builds/5/build.xml", Epoch: 1}},
		LogNextSeq: 42,
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if len(rec.sent) != 1 || rec.sent[0].ResumeBuild == nil {
		t.Fatalf("expected one resume command, got %+v", rec.sent)
	}
	if got := rec.sent[0].ResumeBuild.LogNextSeq; got != 42 {
		t.Fatalf("log_next_seq = %d, want 42", got)
	}
}

// TestHandleLedgerAction_ReleaseAgents proves a release_agents action
// hands the tenant, its agent namespace, and the run's pod names to the
// operator's release routine, and sends no imperative command.
func TestHandleLedgerAction_ReleaseAgents(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionReleaseAgents, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a", RunID: "run-9",
		Tenant: "sbsoftware", AgentNamespace: "sbsoftware",
		AgentPods: []string{"sbsoftware-deploy-phnq4", "sbsoftware-python-3pzkf"},
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if rec.releaseCalls != 1 {
		t.Fatalf("ReleaseRunAgentPods calls = %d, want 1", rec.releaseCalls)
	}
	if rec.releasedTeam != "sbsoftware" || rec.releasedNamespace != "sbsoftware" {
		t.Fatalf("released team/namespace = %q/%q", rec.releasedTeam, rec.releasedNamespace)
	}
	if !reflect.DeepEqual(rec.releasedNames, action.AgentPods) {
		t.Fatalf("released names = %v, want %v", rec.releasedNames, action.AgentPods)
	}
	if len(rec.sent) != 0 {
		t.Fatalf("release_agents sent %d imperative commands, want 0", len(rec.sent))
	}
}

// TestHandleLedgerAction_ReleaseAgentsWithoutPodsDoesNothing proves an
// action that names no tenant, namespace, or pod calls no release.
func TestHandleLedgerAction_ReleaseAgentsWithoutPodsDoesNothing(t *testing.T) {
	cases := map[string]recordstore.LedgerAction{
		"no pods":      {Tenant: "sbsoftware", AgentNamespace: "sbsoftware"},
		"no tenant":    {AgentNamespace: "sbsoftware", AgentPods: []string{"p"}},
		"no namespace": {Tenant: "sbsoftware", AgentPods: []string{"p"}},
	}
	for name, action := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &fakeLedgerActionRunner{}
			action.Kind = recordstore.LedgerActionReleaseAgents
			action.Cluster = "core"
			action.Namespace = "cells-ns"
			action.Controller = "cell-a"
			data, _ := json.Marshal(action)
			handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)
			if rec.releaseCalls != 0 {
				t.Fatalf("ReleaseRunAgentPods calls = %d, want 0", rec.releaseCalls)
			}
		})
	}
}

// TestHandleLedgerAction_ReleaseInstanceAgents proves a release_instance_agents
// action hands the tenant, its agent namespace, the keep set, and the
// confirmation time to the operator's release routine, and sends no
// imperative command.
func TestHandleLedgerAction_ReleaseInstanceAgents(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	before := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionReleaseInstanceAgents, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a", Instance: "pod-old",
		Tenant: "sbsoftware", AgentNamespace: "sbsoftware",
		KeepPods:        []string{"a", "c"},
		RefreshedBefore: &before,
	}
	data, _ := json.Marshal(action)
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)

	if rec.instanceReleaseCalls != 1 {
		t.Fatalf("ReleaseInstanceAgentPods calls = %d, want 1", rec.instanceReleaseCalls)
	}
	if rec.instanceTeam != "sbsoftware" || rec.instanceNamespace != "sbsoftware" {
		t.Fatalf("released team/namespace = %q/%q", rec.instanceTeam, rec.instanceNamespace)
	}
	if !reflect.DeepEqual(rec.instanceKeep, action.KeepPods) {
		t.Fatalf("keep = %v, want %v", rec.instanceKeep, action.KeepPods)
	}
	if !rec.instanceBefore.Equal(before) {
		t.Fatalf("refreshed before = %v, want %v", rec.instanceBefore, before)
	}
	if len(rec.sent) != 0 {
		t.Fatalf("release_instance_agents sent %d imperative commands, want 0", len(rec.sent))
	}
}

// TestHandleLedgerAction_ReleaseInstanceAgentsIncompleteDoesNothing proves an
// action with no tenant, no namespace, or no confirmation time calls no
// release. An empty keep set is valid, so that action does call once.
func TestHandleLedgerAction_ReleaseInstanceAgentsIncompleteDoesNothing(t *testing.T) {
	before := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	cases := map[string]recordstore.LedgerAction{
		"no tenant":       {AgentNamespace: "sbsoftware", RefreshedBefore: &before},
		"no namespace":    {Tenant: "sbsoftware", RefreshedBefore: &before},
		"no confirmation": {Tenant: "sbsoftware", AgentNamespace: "sbsoftware"},
	}
	for name, action := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &fakeLedgerActionRunner{}
			action.Kind = recordstore.LedgerActionReleaseInstanceAgents
			action.Cluster = "core"
			action.Namespace = "cells-ns"
			action.Controller = "cell-a"
			data, _ := json.Marshal(action)
			handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)
			if rec.instanceReleaseCalls != 0 {
				t.Fatalf("ReleaseInstanceAgentPods calls = %d, want 0", rec.instanceReleaseCalls)
			}
		})
	}

	t.Run("empty keep set still releases", func(t *testing.T) {
		rec := &fakeLedgerActionRunner{}
		action := recordstore.LedgerAction{
			Kind: recordstore.LedgerActionReleaseInstanceAgents, Cluster: "core",
			Namespace: "cells-ns", Controller: "cell-a",
			Tenant: "sbsoftware", AgentNamespace: "sbsoftware", RefreshedBefore: &before,
		}
		data, _ := json.Marshal(action)
		handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t), "varroa.ledger.actions.core", "core", rec, nil, data)
		if rec.instanceReleaseCalls != 1 {
			t.Fatalf("ReleaseInstanceAgentPods calls = %d, want 1", rec.instanceReleaseCalls)
		}
	})
}
