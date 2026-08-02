package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSearchWithoutAuthorizerReturnsNoInventory(t *testing.T) {
	server := NewServer(&Dependencies{})
	request := httptest.NewRequest("GET", "/api/v1/search?q=controller", nil)
	response := httptest.NewRecorder()

	server.handleSearch(response, request)

	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"items":[]}` {
		t.Fatalf("response = %d %s, want empty items", response.Code, response.Body.String())
	}
}

func TestRankSearchResultsOrdersAndCapsEachKind(t *testing.T) {
	items := []searchResult{{Type: "group", Name: "query-team"}, {Type: "controller", Name: "z-query"}, {Type: "controller", Name: "query"}, {Type: "controller", Name: "query-a"}, {Type: "controller", Name: "query-b"}, {Type: "controller", Name: "query-c"}, {Type: "controller", Name: "query-d"}, {Type: "controller", Name: "query-e"}, {Type: "namespace", Name: "query-ns"}}
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
	items := map[string]searchResult{"core\x00team": {Type: "namespace", Cluster: "core", Name: "team"}, "edge\x00team": {Type: "namespace", Cluster: "edge", Name: "team"}}
	if got := len(mapSearchResults(items)); got != 2 {
		t.Fatalf("got %d namespaces, want 2", got)
	}
}
