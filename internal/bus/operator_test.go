package bus

import (
	"encoding/json"
	"testing"
)

// TestOperatorClusterSubject verifies OperatorClusterSubject produces
// the expected subject strings for drain and draincancel verbs.
func TestOperatorClusterSubject(t *testing.T) {
	tests := []struct {
		cluster string
		verb    string
		want    string
	}{
		{"dev-cluster", "drain", "operator.dev-cluster.cluster.drain"},
		{"dev-cluster", "draincancel", "operator.dev-cluster.cluster.draincancel"},
		{"core", "drain", "operator.core.cluster.drain"},
	}
	for _, tt := range tests {
		got := OperatorClusterSubject(tt.cluster, tt.verb)
		if got != tt.want {
			t.Errorf("OperatorClusterSubject(%q, %q) = %q, want %q", tt.cluster, tt.verb, got, tt.want)
		}
	}
}

// TestControllersUpdateRoundTrip verifies marshal/unmarshal round-trips for the
// new ControllersUpdateRequest and ControllersUpdateResponse fields (FieldManager,
// Force, Conflicts) added in the SSA write path.
func TestControllersUpdateRoundTrip(t *testing.T) {
	// Request with FieldManager and Force.
	req := ControllersUpdateRequest{
		Namespace:    "ns1",
		Name:         "ctrl1",
		Patch:        json.RawMessage(`{"spec":{"version":"2.0"}}`),
		FieldManager: "varroa-ui",
		Force:        true,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var gotReq ControllersUpdateRequest
	if err := json.Unmarshal(data, &gotReq); err != nil {
		t.Fatal(err)
	}
	if gotReq.FieldManager != "varroa-ui" {
		t.Errorf("FieldManager = %q, want %q", gotReq.FieldManager, "varroa-ui")
	}
	if !gotReq.Force {
		t.Error("Force = false, want true")
	}

	// Response with Conflicts.
	resp := ControllersUpdateResponse{
		Item:  json.RawMessage(`{"spec":{"version":"2.0"}}`),
		Code:  CodeConflict,
		Error: "field conflict",
		Conflicts: []FieldConflict{
			{Field: ".spec.resources", Manager: "other-manager", Message: `conflict with "other-manager"`},
			{Field: ".spec.version", Manager: "kubectl", Message: `conflict with "kubectl"`},
		},
	}
	data, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var gotResp ControllersUpdateResponse
	if err := json.Unmarshal(data, &gotResp); err != nil {
		t.Fatal(err)
	}
	if gotResp.Code != CodeConflict {
		t.Errorf("Code = %q, want %q", gotResp.Code, CodeConflict)
	}
	if len(gotResp.Conflicts) != 2 {
		t.Fatalf("len(Conflicts) = %d, want 2", len(gotResp.Conflicts))
	}
	if gotResp.Conflicts[0].Field != ".spec.resources" || gotResp.Conflicts[0].Manager != "other-manager" {
		t.Errorf("Conflicts[0] = %+v, want {Field:.spec.resources Manager:other-manager}", gotResp.Conflicts[0])
	}
}
