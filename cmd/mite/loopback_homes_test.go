package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// getHomes calls GET /v1/homes with the loopback bearer token and decodes
// the body.
func getHomes(t *testing.T, agent *Agent, port int) homesResponse {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/homes", port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the homes request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/homes: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out homesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the homes body: %v", err)
	}
	return out
}

// TestHomesAnswersTheAppliedMap asserts the route serves what the newest
// applied desired state carried, and that the mite computes nothing.
func TestHomesAnswersTheAppliedMap(t *testing.T) {
	agent, port := startTestLoopback(t)

	agent.cacheTenantHomes(&mitev1.DesiredStateCommand{
		CommandId: "cmd-1",
		TenantHomes: []*mitev1.TenantHomes{
			{Tenant: "payments", Slot: 1, Role: "active", JobHomes: map[string]int{"payments/api": 1, "payments/web": 0}},
			{Tenant: "search", Slot: 0, Role: "standby"},
		},
	})

	got := getHomes(t, agent, port)
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(got.Entries), got.Entries)
	}
	payments := got.Entries[0]
	if payments.Tenant != "payments" || payments.Slot != 1 || payments.Role != "active" {
		t.Fatalf("the first entry = %+v, want payments on slot 1 as active", payments)
	}
	if payments.JobHomes["payments/api"] != 1 || payments.JobHomes["payments/web"] != 0 {
		t.Fatalf("job homes = %v, want payments/api on 1 and payments/web on 0", payments.JobHomes)
	}
	if got.Entries[1].Role != "standby" {
		t.Fatalf("the second entry = %+v, want the role standby", got.Entries[1])
	}
}

// TestHomesNeedsTheBearerToken asserts a call with no token answers 401
// and no body.
func TestHomesNeedsTheBearerToken(t *testing.T) {
	_, port := startTestLoopback(t)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/homes", port)) //nolint:noctx // a loopback test call
	if err != nil {
		t.Fatalf("GET /v1/homes: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHomesAnswersEmptyBeforeAnApply asserts a mite that has applied no
// desired state carrying the field answers an empty entry list. A caller
// reads that as "no opinion" and infers no home from it.
func TestHomesAnswersEmptyBeforeAnApply(t *testing.T) {
	agent, port := startTestLoopback(t)

	got := getHomes(t, agent, port)
	if got.Entries == nil {
		t.Fatal("the body carries a null entry list, want an empty array")
	}
	if len(got.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", got.Entries)
	}
}

// TestHomesRefusesAWriteMethod asserts the route is read-only.
func TestHomesRefusesAWriteMethod(t *testing.T) {
	agent, port := startTestLoopback(t)

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/homes", port)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/homes: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestTenantHomesProtoRoundTrip asserts the generated message survives a
// JSON round trip with its map field intact.
func TestTenantHomesProtoRoundTrip(t *testing.T) {
	cmd := &mitev1.DesiredStateCommand{
		CommandId: "cmd-1",
		TenantHomes: []*mitev1.TenantHomes{
			{Tenant: "payments", Slot: 2, Role: "active", JobHomes: map[string]int{"payments/api": 2}},
		},
	}
	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back mitev1.DesiredStateCommand
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.TenantHomes) != 1 {
		t.Fatalf("entries = %d, want 1", len(back.TenantHomes))
	}
	entry := back.TenantHomes[0]
	if entry.Tenant != "payments" || entry.Slot != 2 || entry.Role != "active" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.JobHomes["payments/api"] != 2 {
		t.Fatalf("job homes = %v, want payments/api on 2", entry.JobHomes)
	}
}
