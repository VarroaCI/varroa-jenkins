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
