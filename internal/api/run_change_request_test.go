package api

import (
	"encoding/json"
	"testing"
)

// TestChangeRequestOfReadsStartedPayload proves the reader maps every
// change_request key of the run.started payload onto the camel-case shape.
func TestChangeRequestOfReadsStartedPayload(t *testing.T) {
	payload := json.RawMessage(`{"run.started":{"payload":{"change_request":{"id":"12","title":"Fix","source_branch":"fix/x","target_branch":"main","url":"https://forge.example/pr/12"}}}}`)
	got := changeRequestOf(payload)
	if got == nil {
		t.Fatal("changeRequestOf = nil, want the change request")
	}
	want := changeRequestJSON{ID: "12", Title: "Fix", SourceBranch: "fix/x", TargetBranch: "main", URL: "https://forge.example/pr/12"}
	if *got != want {
		t.Fatalf("changeRequestOf = %+v, want %+v", *got, want)
	}
	if changeRequestOf(json.RawMessage(`{"run.started":{"payload":{"branch":"main"}}}`)) != nil {
		t.Fatal("changeRequestOf returned a value for a branch run")
	}
}

// TestChangeRequestOfMalformed proves the reader answers nil for an empty,
// malformed, or id-less payload.
func TestChangeRequestOfMalformed(t *testing.T) {
	for name, payload := range map[string]json.RawMessage{
		"empty":     nil,
		"malformed": json.RawMessage(`{"run.started":`),
		"no id":     json.RawMessage(`{"run.started":{"payload":{"change_request":{"title":"orphan"}}}}`),
	} {
		if got := changeRequestOf(payload); got != nil {
			t.Errorf("%s: changeRequestOf = %+v, want nil", name, *got)
		}
	}
}
