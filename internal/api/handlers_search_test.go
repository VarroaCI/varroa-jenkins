package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestHandleSearchWithoutAuthorizerReturnsNoInventory(t *testing.T) {
	server := NewServer(&Dependencies{})
	request := httptest.NewRequest("GET", "/api/v1/search?q=controller", nil)
	response := httptest.NewRecorder()

	server.handleSearch(response, request)

	if response.Code != 200 {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var body struct {
		Items    []SearchResult        `json:"items"`
		Clusters []ClusterFanoutStatus `json:"clusters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	if body.Items == nil || len(body.Items) != 0 {
		t.Fatalf("items = %s, want []", response.Body.String())
	}
	if body.Clusters == nil || len(body.Clusters) != 0 {
		t.Fatalf("clusters = %s, want []", response.Body.String())
	}
}

func TestRankSearchResultsOrdersAndCapsEachKind(t *testing.T) {
	items := []SearchResult{{Type: "group", Name: "query-team"}, {Type: "controller", Name: "z-query"}, {Type: "controller", Name: "query"}, {Type: "controller", Name: "query-a"}, {Type: "controller", Name: "query-b"}, {Type: "controller", Name: "query-c"}, {Type: "controller", Name: "query-d"}, {Type: "controller", Name: "query-e"}, {Type: "namespace", Name: "query-ns"}}
	got := rankSearchResults(items, "query")
	if len(got) != 7 {
		t.Fatalf("got %d results, want 7", len(got))
	}
	if got[0].Type != "controller" || got[0].Name != "query" {
		t.Fatalf("first result = %#v, want exact controller match", got[0])
	}
	if got[5].Type != "namespace" || got[6].Type != "group" {
		t.Fatalf("kind order = %q, %q", got[5].Type, got[6].Type)
	}
}

func TestSearchControllerLinkEscapesIdentity(t *testing.T) {
	got := searchControllerLink("edge/a", "team one", "controller#1")
	want := "/controllers/edge%2Fa/team%20one/controller%231"
	if got != want {
		t.Fatalf("link = %q, want %q", got, want)
	}
}

func TestMapSearchResultsDeduplicatesIdentity(t *testing.T) {
	items := map[string]SearchResult{"core\x00team": {Type: "namespace", Cluster: "core", Name: "team"}, "edge\x00team": {Type: "namespace", Cluster: "edge", Name: "team"}}
	if got := len(mapSearchResults(items)); got != 2 {
		t.Fatalf("got %d namespaces, want 2", got)
	}
}

// TestSearch_CatalogItemsFilteredByNamespace pins down that global search
// returns only the catalog items the caller can read: its own namespace and
// the operator namespace, and no other tenant's namespace.
func TestSearch_CatalogItemsFilteredByNamespace(t *testing.T) {
	item := func(ns, name string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"name": name, "namespace": ns, "description": "shared tool"})
		return b
	}
	brood := &scopeTestConfigBrood{
		operatorNs: "varroa-system",
		items: []json.RawMessage{
			item("team-a", "shared-a"),
			item("team-b", "shared-b"),
			item("varroa-system", "shared-platform"),
		},
	}
	server := NewServer(&Dependencies{
		Authorizer:        scopedDeveloperAuthorizer(),
		ConfigBrood:       brood,
		Brood:             &stubBrood{localCluster: "core"},
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})

	resp, err := server.Search(context.Background(), scopedDevClaims, "shared", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, r := range resp.Items {
		if r.Type == "catalogitem" {
			got[r.Namespace] = true
		}
	}
	if !got["team-a"] || !got["varroa-system"] || got["team-b"] {
		t.Errorf("catalog item namespaces = %v, want team-a and varroa-system only", got)
	}
}
