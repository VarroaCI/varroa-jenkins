package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// TestTeam_EditBufferExcludesStatusFields tests that the team edit buffer
// excludes status-ish fields and PUTs a projection of writable fields.
func TestTeam_EditBufferExcludesStatus(t *testing.T) {
	testSetup(t)
	callCount := 0
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// GET detail — return full teamEntry with status fields
			if r.Method != "GET" {
				t.Errorf("call 1: expected GET, got %s", r.Method)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"name":                "team-alpha",
				"displayName":         "Team Alpha",
				"roleRef":             "admin",
				"members":             []any{"alice", "bob"},
				"namespaces":          []any{"ns1"},
				"provisionNamespaces": true,
				"groupRef":            "g-123",
				"bindingRef":          "b-456",
				"namespaceStates":     []any{},
				"conditions":          []any{},
				"observedGeneration":  float64(1),
			})
		} else {
			// PUT — should only have writable fields
			if r.Method != "PUT" {
				t.Errorf("call 2: expected PUT, got %s", r.Method)
			}
			// Check body doesn't contain status-ish fields
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["groupRef"]; ok {
				t.Error("PUT body should not contain groupRef")
			}
			if _, ok := body["conditions"]; ok {
				t.Error("PUT body should not contain conditions")
			}
			if _, ok := body["namespaceStates"]; ok {
				t.Error("PUT body should not contain namespaceStates")
			}
			// Writable fields should be present
			if _, ok := body["name"]; !ok {
				t.Error("PUT body should contain name")
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = func(path string) error {
		// Write edited content with a change (change roleRef to viewer)
		content := "name: team-alpha\ndisplayName: Team Alpha\nroleRef: viewer\nmembers:\n  - alice\n  - bob\nnamespaces:\n  - ns1\nprovisionNamespaces: true\n"
		return os.WriteFile(path, []byte(content), 0644)
	}
	defer func() { runEditorC3 = origEditor }()

	root := newRootCmd()
	root.SetArgs([]string{"edit", "team", "team-alpha"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected 2 calls (GET + PUT), got %d", callCount)
	}
}

// TestTeam_GetList tests listing teams
func TestTeam_GetList(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":        "team-alpha",
					"displayName": "Team Alpha",
					"roleRef":     "admin",
					"members":     []any{"alice"},
					"namespaces":  []any{"ns1"},
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "teams"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
