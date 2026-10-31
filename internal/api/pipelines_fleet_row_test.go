package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func TestFleetTriggerMapping(t *testing.T) {
	pr := &changeRequestJSON{ID: "178"}
	upstreamJob := "pay/build"
	upstreamNumber := int64(7)
	cases := []struct {
		name      string
		source    string
		cr        *changeRequestJSON
		run       recordstore.Run
		wantKind  string
		wantLabel string
	}{
		{"user", triggerSourceUser, nil, recordstore.Run{}, triggerKindManual, "Manual"},
		{"webhook without change request", triggerSourceWebhook, nil, recordstore.Run{}, triggerKindWebhook, "Webhook"},
		{"webhook with change request", triggerSourceWebhook, pr, recordstore.Run{}, triggerKindPullRequest, "Pull request #178"},
		{"scm without change request", triggerSourceSCM, nil, recordstore.Run{}, triggerKindPush, "Push"},
		{"scm with change request", triggerSourceSCM, pr, recordstore.Run{}, triggerKindPullRequest, "Pull request #178"},
		{"cron", triggerSourceCron, nil, recordstore.Run{}, triggerKindCron, "Cron"},
		{"upstream without columns", triggerSourceUpstream, nil, recordstore.Run{}, triggerKindUpstream, "Upstream"},
		{"upstream with job and number", triggerSourceUpstream, nil,
			recordstore.Run{UpstreamJob: &upstreamJob, UpstreamNumber: &upstreamNumber},
			triggerKindUpstream, "Upstream · pay/build #7"},
		{"replay", triggerSourceReplay, nil, recordstore.Run{}, triggerKindReplay, "Replay"},
		{"restart", triggerSourceRestart, nil, recordstore.Run{}, triggerKindRestart, "Restart"},
		{"other", triggerSourceOther, nil, recordstore.Run{}, triggerKindOther, "Other"},
		{"empty source", "", nil, recordstore.Run{}, triggerKindOther, "Other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum := runSummaryJSON{TriggerSource: tc.source, ChangeRequest: tc.cr}
			got := fleetTrigger(tc.run, sum)
			if got.Kind != tc.wantKind || got.Label != tc.wantLabel {
				t.Fatalf("trigger = %+v; want kind %q label %q", got, tc.wantKind, tc.wantLabel)
			}
		})
	}
}

func TestFleetRunTitle(t *testing.T) {
	two, _ := json.Marshal([]map[string]string{
		{"message": "older subject"},
		{"message": "fix the cache key\n\nlong body"},
	})
	run := recordstore.Run{JobPath: "pay/build", Changes: two}
	if got := fleetRunTitle(run); got != "fix the cache key" {
		t.Fatalf("title = %q; want the newest first line", got)
	}
	if got := fleetRunTitle(recordstore.Run{JobPath: "pay/build"}); got != "pay/build" {
		t.Fatalf("title with no changes = %q; want the job path", got)
	}
}

func TestBuildFleetRunRowCarriesChangeSummary(t *testing.T) {
	two, _ := json.Marshal([]map[string]string{
		{"message": "older subject"},
		{"message": "fix the cache key\n\nbody"},
	})
	row := buildFleetRunRow(recordstore.Run{RunID: "run-1", JobPath: "pay/build", Status: "COMPLETED", Changes: two}, "pay", false)
	if row.ChangeSummary != "fix the cache key" || row.Title != "fix the cache key" {
		t.Fatalf("changeSummary = %q, title = %q; want both %q", row.ChangeSummary, row.Title, "fix the cache key")
	}

	bare := buildFleetRunRow(recordstore.Run{RunID: "run-2", JobPath: "pay/build", Status: "COMPLETED"}, "pay", false)
	if bare.ChangeSummary != "" || bare.Title != "pay/build" {
		t.Fatalf("no changes: changeSummary = %q, title = %q; want empty and the job path", bare.ChangeSummary, bare.Title)
	}
}

func TestBuildFleetRunRowDerivedFields(t *testing.T) {
	started := time.Date(2026, 10, 30, 10, 0, 1, 500_000_000, time.UTC)
	queued := started.Add(-1500 * time.Millisecond)
	interrupted, rail := "INTERRUPTED", "pay"
	upstreamTenant, upstreamJob := "ops", "ops/deploy"
	upstreamNumber := int64(4)
	actor := "nathan"
	running := recordstore.Run{
		RunID: "0192f000-0000-7000-8000-000000000001", Cluster: "core", Tenant: "pay", JobPath: "pay/api",
		Status: "RUNNING", Actor: &actor, QueuedAt: &queued, StartedAt: &started,
		Inputs: json.RawMessage(`[{"id":"a"}]`), CreatedAt: started,
	}
	row := buildFleetRunRow(running, rail, true)
	if !row.PendingInput {
		t.Error("RUNNING with one input: pendingInput = false; want true")
	}
	if row.QueueWaitMs == nil || *row.QueueWaitMs != 1500 {
		t.Errorf("queueWaitMs = %v; want 1500", row.QueueWaitMs)
	}
	if row.Tenant != rail || row.Cluster != "core" || !row.CanWrite {
		t.Errorf("tenant/cluster/canWrite = %q/%q/%v; want %q/core/true", row.Tenant, row.Cluster, row.CanWrite, rail)
	}
	if row.TenantFolder != "pay" {
		t.Errorf("tenantFolder = %q; want pay", row.TenantFolder)
	}

	completed := running
	completed.Status = "COMPLETED"
	if buildFleetRunRow(completed, rail, false).PendingInput {
		t.Error("COMPLETED with one input: pendingInput = true; want false")
	}

	stopped := recordstore.Run{
		RunID: "0192f000-0000-7000-8000-000000000002", Cluster: "core", JobPath: "pay/api",
		Status: "COMPLETED", Result: &interrupted, Payload: json.RawMessage(`{"interrupted_reason":"cell-lost"}`),
		UpstreamTenant: &upstreamTenant, UpstreamJob: &upstreamJob, UpstreamNumber: &upstreamNumber,
	}
	row = buildFleetRunRow(stopped, rail, false)
	if row.InterruptedReason != "cell-lost" {
		t.Errorf("interruptedReason = %q; want cell-lost", row.InterruptedReason)
	}
	if row.Upstream == nil || row.Upstream.Tenant != "ops" || row.Upstream.Job != "ops/deploy" || row.Upstream.BuildNumber != 4 {
		t.Errorf("upstream = %+v; want ops, ops/deploy, 4", row.Upstream)
	}
	if row.CanWrite {
		t.Error("canWrite = true; want false")
	}
}

func TestBuildFleetRunRowTenantFolder(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		want   string
	}{
		{"plain tenant names its own folder", "pay", "pay"},
		{"controller tenant has no folder", "ns/ctrl", ""},
		{"empty tenant has no folder", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := recordstore.Run{Cluster: "core", Tenant: tc.tenant, JobPath: "pay/api", Status: "COMPLETED"}
			if got := buildFleetRunRow(run, tc.tenant, false).TenantFolder; got != tc.want {
				t.Errorf("tenantFolder = %q; want %q", got, tc.want)
			}
		})
	}
}
