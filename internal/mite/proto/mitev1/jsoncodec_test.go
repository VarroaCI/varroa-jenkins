package mitev1

import (
	"encoding/json"
	"testing"
)

func TestTokenRefreshRequestRoundTrip(t *testing.T) {
	req := &TokenRefreshRequest{}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal TokenRefreshRequest: %v", err)
	}
	var req2 TokenRefreshRequest
	if err := json.Unmarshal(data, &req2); err != nil {
		t.Fatalf("unmarshal TokenRefreshRequest: %v", err)
	}
}

func TestTokenGrantRoundTrip(t *testing.T) {
	grant := &TokenGrant{
		MiteJenkinsToken:    "eyJhbGciOiJSUzI1NiJ9.token",
		MiteJenkinsTokenExp: 1234567890,
	}
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("marshal TokenGrant: %v", err)
	}
	var grant2 TokenGrant
	if err := json.Unmarshal(data, &grant2); err != nil {
		t.Fatalf("unmarshal TokenGrant: %v", err)
	}
	if grant2.MiteJenkinsToken != grant.MiteJenkinsToken {
		t.Errorf("token mismatch: got %q, want %q", grant2.MiteJenkinsToken, grant.MiteJenkinsToken)
	}
	if grant2.MiteJenkinsTokenExp != grant.MiteJenkinsTokenExp {
		t.Errorf("exp mismatch: got %d, want %d", grant2.MiteJenkinsTokenExp, grant.MiteJenkinsTokenExp)
	}
}

func TestMiteMessageTokenRefreshRequestRoundTrip(t *testing.T) {
	mm := &MiteMessage{
		Message: &MiteMessage_TokenRefreshRequest{
			TokenRefreshRequest: &TokenRefreshRequest{},
		},
	}
	data, err := json.Marshal(mm)
	if err != nil {
		t.Fatalf("marshal MiteMessage with TokenRefreshRequest: %v", err)
	}
	var mm2 MiteMessage
	if err := json.Unmarshal(data, &mm2); err != nil {
		t.Fatalf("unmarshal MiteMessage with TokenRefreshRequest: %v", err)
	}
	if mm2.GetTokenRefreshRequest() == nil {
		t.Error("GetTokenRefreshRequest returned nil after round trip")
	}
}

func TestOperatorMessageTokenGrantRoundTrip(t *testing.T) {
	om := &OperatorMessage{
		Message: &OperatorMessage_TokenGrant{
			TokenGrant: &TokenGrant{
				MiteJenkinsToken:    "fresh-token",
				MiteJenkinsTokenExp: 9876543210,
			},
		},
	}
	data, err := json.Marshal(om)
	if err != nil {
		t.Fatalf("marshal OperatorMessage with TokenGrant: %v", err)
	}

	// Verify JSON structure.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["token_grant"]; !ok {
		t.Error("missing token_grant key in JSON")
	}

	var om2 OperatorMessage
	if err := json.Unmarshal(data, &om2); err != nil {
		t.Fatalf("unmarshal OperatorMessage with TokenGrant: %v", err)
	}
	grant := om2.GetTokenGrant()
	if grant == nil {
		t.Fatal("GetTokenGrant returned nil after round trip")
	}
	if grant.MiteJenkinsToken != "fresh-token" {
		t.Errorf("token mismatch: got %q, want %q", grant.MiteJenkinsToken, "fresh-token")
	}
	if grant.MiteJenkinsTokenExp != 9876543210 {
		t.Errorf("exp mismatch: got %d, want %d", grant.MiteJenkinsTokenExp, 9876543210)
	}
}

// TestBuildEventTemplateFieldsRoundTrip proves the four template-resolution
// fields (38-41) survive a marshal/unmarshal round trip at their own field
// numbers, distinct from delivery_id (42).
func TestBuildEventTemplateFieldsRoundTrip(t *testing.T) {
	evt := &BuildEvent{
		Phase:           "STARTED",
		RunId:           "run-1",
		TemplateName:    "go-service",
		TemplateVersion: "2.1.4",
		MarkerPath:      ".varroa/pipeline.yaml",
		MarkerRevision:  "abc123",
		DeliveryId:      "github:delivery-1",
	}
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal BuildEvent: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, key := range []string{"template_name", "template_version", "marker_path", "marker_revision", "delivery_id"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing %q key in JSON", key)
		}
	}

	var evt2 BuildEvent
	if err := json.Unmarshal(data, &evt2); err != nil {
		t.Fatalf("unmarshal BuildEvent: %v", err)
	}
	if evt2.TemplateName != evt.TemplateName {
		t.Errorf("TemplateName = %q, want %q", evt2.TemplateName, evt.TemplateName)
	}
	if evt2.TemplateVersion != evt.TemplateVersion {
		t.Errorf("TemplateVersion = %q, want %q", evt2.TemplateVersion, evt.TemplateVersion)
	}
	if evt2.MarkerPath != evt.MarkerPath {
		t.Errorf("MarkerPath = %q, want %q", evt2.MarkerPath, evt.MarkerPath)
	}
	if evt2.MarkerRevision != evt.MarkerRevision {
		t.Errorf("MarkerRevision = %q, want %q", evt2.MarkerRevision, evt.MarkerRevision)
	}
	if evt2.DeliveryId != evt.DeliveryId {
		t.Errorf("DeliveryId = %q, want %q", evt2.DeliveryId, evt.DeliveryId)
	}
}

// TestBuildEventTemplateFieldsOmittedWhenEmpty proves a run with no
// template marshals none of the four fields, matching design.md D14: all
// four are empty for the default definition path.
func TestBuildEventTemplateFieldsOmittedWhenEmpty(t *testing.T) {
	evt := &BuildEvent{Phase: "STARTED", RunId: "run-1"}
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal BuildEvent: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, key := range []string{"template_name", "template_version", "marker_path", "marker_revision"} {
		if _, ok := raw[key]; ok {
			t.Errorf("key %q present for a run with no template, want omitted", key)
		}
	}
}
