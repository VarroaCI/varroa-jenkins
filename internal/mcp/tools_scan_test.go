package mcp

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestScanMultibranchToolPublishesOneScan asserts the tool publishes one
// SCAN_MULTIBRANCH command for the job and answers the scan identifier with
// the requested state.
func TestScanMultibranchToolPublishesOneScan(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("write"),
		Store:          crdstore.NewFake(),
		RecordStore:    newFakePipelineReader(),
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}}},
		MiteRegistry:   sent,
	}

	tr := callPipelineTool(t, deps, "scan_multibranch", map[string]interface{}{
		"tenant": "team-a", "job": "team-a/multi",
	})
	if tr.IsError {
		t.Fatalf("scan_multibranch returned error: %v", tr.Content)
	}
	var payload struct {
		ScanID string `json:"scanId"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.ScanID == "" || payload.State != "requested" {
		t.Fatalf("result = %+v, want a scan identifier and the requested state", payload)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("published %d commands, want 1", len(sent.sent))
	}
	cmd := sent.sent[0]
	if cmd.Type != mitev1.CommandTypeScanMultibranch || cmd.ScanMultibranch == nil ||
		cmd.ScanMultibranch.ScanId != payload.ScanID || cmd.ScanMultibranch.JobPath != "team-a/multi" {
		t.Fatalf("command = %+v, want one SCAN_MULTIBRANCH for the job", cmd)
	}
}

// TestScanMultibranchToolDeniesAReadOnlyCaller asserts a caller without the
// pipelines write verb publishes nothing.
func TestScanMultibranchToolDeniesAReadOnlyCaller(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    newFakePipelineReader(),
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}}},
		MiteRegistry:   sent,
	}

	tr := callPipelineTool(t, deps, "scan_multibranch", map[string]interface{}{
		"tenant": "team-a", "job": "team-a/multi",
	})
	if !tr.IsError {
		t.Fatal("expected scan_multibranch to deny a read-only caller")
	}
	if len(sent.sent) != 0 {
		t.Fatalf("published %d commands, want 0", len(sent.sent))
	}
}
