package mcp

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestListVersionCandidates_UpstreamMetadata pins the envelope of
// list_version_candidates. With upstream off and no candidates, both the
// summary path and the verbose path return an empty items array and the state.
func TestListVersionCandidates_UpstreamMetadata(t *testing.T) {
	claims := &auth.Claims{Subject: "admin", Groups: []string{"admins"}}
	deps := &api.Dependencies{
		Authorizer:      guardAuthorizer(),
		Store:           crdstore.NewFake(),
		JenkinsUpstream: controller.JenkinsUpstreamOff,
	}

	for _, verbose := range []bool{false, true} {
		args := map[string]interface{}{}
		if verbose {
			args["verbose"] = true
		}
		tr := callToolAs(t, deps, claims, "list_version_candidates", args)
		if tr.IsError {
			t.Fatalf("verbose=%v: tool error: %s", verbose, tr.Content[0].Text)
		}
		var out struct {
			Items            json.RawMessage `json:"items"`
			Count            int             `json:"count"`
			UpstreamMetadata string          `json:"upstreamMetadata"`
		}
		if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
			t.Fatalf("verbose=%v: decode: %v", verbose, err)
		}
		if out.UpstreamMetadata != "off" {
			t.Errorf("verbose=%v: upstreamMetadata = %q, want off", verbose, out.UpstreamMetadata)
		}
		if out.Count != 0 {
			t.Errorf("verbose=%v: count = %d, want 0", verbose, out.Count)
		}
		if string(out.Items) != "[]" {
			t.Errorf("verbose=%v: items = %s, want []", verbose, out.Items)
		}
	}
}
