package recordstore

import (
	"encoding/json"
	"testing"
	"time"
)

// TestClaimRequestJSONFieldNames asserts ClaimRequest marshals every
// field under the exact JSON name D2 pins, and round-trips.
func TestClaimRequestJSONFieldNames(t *testing.T) {
	req := ClaimRequest{
		IntentKey:              "manual:" + "11111111-1111-7111-8111-111111111111",
		RunID:                  "11111111-1111-7111-8111-111111111111",
		Cluster:                "core",
		Namespace:              "jenkins",
		Cell:                   "cell-a",
		CellInstance:           "pod-uid-1",
		Tenant:                 "acme",
		JobPath:                "acme/app",
		Branch:                 "main",
		Actor:                  "alice",
		Cause:                  "manual",
		ParamsDigest:           "sha256:deadbeef",
		PlacementGeneration:    3,
		JenkinsNextBuildNumber: 9,
		RequestedAt:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}

	wantFields := []string{
		"intent_key", "run_id", "cluster", "namespace", "cell", "cell_instance",
		"tenant", "job_path", "branch", "actor", "cause", "params_digest",
		"placement_generation", "jenkins_next_build_number", "requested_at",
	}
	for _, field := range wantFields {
		if _, ok := raw[field]; !ok {
			t.Fatalf("marshaled ClaimRequest is missing field %q: %s", field, data)
		}
	}
	if len(raw) != len(wantFields) {
		t.Fatalf("marshaled ClaimRequest has %d fields, want %d: %s", len(raw), len(wantFields), data)
	}

	var round ClaimRequest
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal into ClaimRequest: %v", err)
	}
	round.RequestedAt = round.RequestedAt.UTC()
	if round != req {
		t.Fatalf("round trip mismatch: want %+v, got %+v", req, round)
	}
}

// TestClaimResponseJSONFieldNames asserts ClaimResponse marshals every
// field under the exact JSON name D2 pins, and round-trips.
func TestClaimResponseJSONFieldNames(t *testing.T) {
	resp := ClaimResponse{
		Decision:    DecisionGranted,
		RunID:       "11111111-1111-7111-8111-111111111111",
		BuildNumber: 7,
		ReasonCode:  ReasonFenced,
		Message:     "message text",
		Error:       "error text",
		Code:        CodeInternal,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}

	wantFields := []string{"decision", "run_id", "build_number", "reason_code", "message", "error", "code"}
	for _, field := range wantFields {
		if _, ok := raw[field]; !ok {
			t.Fatalf("marshaled ClaimResponse is missing field %q: %s", field, data)
		}
	}
	if len(raw) != len(wantFields) {
		t.Fatalf("marshaled ClaimResponse has %d fields, want %d: %s", len(raw), len(wantFields), data)
	}

	var round ClaimResponse
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal into ClaimResponse: %v", err)
	}
	if round != resp {
		t.Fatalf("round trip mismatch: want %+v, got %+v", resp, round)
	}
}
