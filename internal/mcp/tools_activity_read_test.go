package mcp

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// list_activity must read through deps.Backfill — the source the REST handler
// uses — never deps.ActivityStore directly. The ring store is only fed in
// retention-off mode, so a tool reading it directly returns an eternally empty
// feed on any stream-mode brood (the live defect this file pins).

func backfillDeps(events ...activity.Event) *api.Dependencies {
	store := activity.New(100)
	for _, e := range events {
		store.Append(e)
	}
	deps := newTestDeps()
	deps.Backfill = activity.NewRingBackfill(store)
	deps.Authorizer = guardAuthorizer()
	return deps
}

func callListActivity(t *testing.T, deps *api.Dependencies, args map[string]interface{}) []activity.Event {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_activity",
		"arguments": args,
	}, guardClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_activity returned error: %v", tr.Content)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("list_activity returned no content")
	}
	var out struct {
		Count int              `json:"count"`
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &out); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	return out.Items
}

func TestListActivityReadsBackfill(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa", Message: "created"},
		activity.Event{Type: "controller.deleted", Controller: "other", Namespace: "varroa", Message: "deleted"},
	)
	events := callListActivity(t, deps, map[string]interface{}{})
	if len(events) != 2 {
		t.Fatalf("expected 2 events from backfill, got %d", len(events))
	}
}

func TestListActivityControllerFilter(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa", Message: "created"},
		activity.Event{Type: "controller.deleted", Controller: "other", Namespace: "varroa", Message: "deleted"},
	)
	events := callListActivity(t, deps, map[string]interface{}{"controller": "auditboi"})
	if len(events) != 1 || events[0].Controller != "auditboi" {
		t.Fatalf("expected only auditboi events, got %+v", events)
	}
}

func TestListActivityRequiresClaimsWhenBackfillPresent(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa"},
	)
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_activity",
		"arguments": map[string]interface{}{},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected authentication error without claims, got %v", tr.Content)
	}
}

func TestSearchMatchesControllersBySubstring(t *testing.T) {
	deps := newTestDeps()
	fake := deps.Store.(*crdstore.Fake)
	if err := fake.Seed(
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "auditboi", Namespace: "varroa"}},
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "testcore", Namespace: "varroa"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "audit", "namespace": "varroa"},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	var page struct {
		Count int                      `json:"count"`
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &page); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0]["name"] != "auditboi" {
		t.Fatalf("expected exactly auditboi, got %+v", page.Items)
	}

	resp = mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "no-such-controller", "namespace": "varroa"},
	}, nil)
	tr = parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &page); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected no hits, got %+v", page.Items)
	}
}

// With an Authorizer configured, search must refuse unauthenticated callers —
// treating missing claims as "no filter" would let them enumerate controller
// names across the brood.
func TestSearchRequiresClaimsWithAuthorizer(t *testing.T) {
	deps := newTestDeps()
	deps.Authorizer = guardAuthorizer()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "anything"},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected authentication error without claims, got %v", tr.Content)
	}
}
