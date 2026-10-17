package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// TestHandleVersionCandidates_UpstreamMetadata pins the upstream state the
// version-candidate list reports. An unset state reports online, the built-in
// host. With no candidates, the item list is an empty array, not null.
func TestHandleVersionCandidates_UpstreamMetadata(t *testing.T) {
	cases := []struct {
		name  string
		state controller.JenkinsUpstreamState
		want  string
	}{
		{name: "unset reports online", state: "", want: "online"},
		{name: "online", state: controller.JenkinsUpstreamOnline, want: "online"},
		{name: "mirror", state: controller.JenkinsUpstreamMirror, want: "mirror"},
		{name: "off", state: controller.JenkinsUpstreamOff, want: "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := newVersionCandidateTestDeps(t)
			deps.JenkinsUpstream = tc.state
			srv := &Server{deps: deps}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/version-candidates", nil)
			req = req.WithContext(contextWithClaims(req.Context(), adminClaims))
			req.URL.Path = "/version-candidates"
			w := httptest.NewRecorder()
			srv.HandleVersionCandidates(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			var body struct {
				Items            json.RawMessage `json:"items"`
				UpstreamMetadata string          `json:"upstreamMetadata"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.UpstreamMetadata != tc.want {
				t.Errorf("upstreamMetadata = %q, want %q", body.UpstreamMetadata, tc.want)
			}
			if string(body.Items) != "[]" {
				t.Errorf("items = %s, want []", body.Items)
			}
		})
	}
}
