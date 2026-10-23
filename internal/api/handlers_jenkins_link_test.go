package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestParseJenkinsItemPath asserts the job path and build number that a
// Jenkins item path under the headless door names.
func TestParseJenkinsItemPath(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantJob   string
		wantBuild int64
	}{
		{"nested job with build and display", "job/a/job/b/12/display/redirect", "a/b", 12},
		{"encoded slash in a branch name decodes once", "job/t/job/r/job/feature%252Fx/3/", "t/r/feature%2Fx", 3},
		{"job only", "job/a/", "a", 0},
		{"lastBuild names no number", "job/a/lastBuild/", "a", 0},
		{"view segment is skipped", "view/all/job/a/5/", "a", 5},
		{"no job", "manage", "", 0},
		{"undecodable segment", "job/%zz/1", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, build := parseJenkinsItemPath(tc.raw)
			if job != tc.wantJob || build != tc.wantBuild {
				t.Errorf("parseJenkinsItemPath(%q) = (%q, %d), want (%q, %d)",
					tc.raw, job, build, tc.wantJob, tc.wantBuild)
			}
		})
	}
}

// callJenkinsLink sends GET (or method) to the jenkins-link handler for the
// controller test-ns/ctrl on cluster core, with the given raw query path.
func callJenkinsLink(t *testing.T, srv *Server, method, rawPath string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/clusters/core/controllers/test-ns/ctrl/jenkins-link"
	if rawPath != "" {
		target += "?path=" + rawPath
	}
	req := httptest.NewRequest(method, target, nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), testClaims()))
	rec := httptest.NewRecorder()
	srv.handleControllerJenkinsLink(rec, req, "core", "test-ns", "ctrl")
	return rec
}

func linkBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out.Link
}

// TestHandleControllerJenkinsLink asserts the link a headless door path
// resolves to, and every failure answer.
func TestHandleControllerJenkinsLink(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	buildNo := int64(12)

	t.Run("cell run answers the build page under the Tenant", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.addRun(recordstore.Run{
			RunID: "run-cell", Cluster: "core", Namespace: "test-ns", Cell: "ctrl",
			Tenant: "team-a", JobPath: "team-a/app", BuildNumber: &buildNo, CreatedAt: base,
		})
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), reader, nil)

		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/12/display/redirect")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if got, want := linkBody(t, rec), "/pipelines/team-a/team-a%2Fapp/run-cell"; got != want {
			t.Errorf("link = %q, want %q", got, want)
		}
	})

	t.Run("dedicated controller answers the job page under the controller name", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.addRun(recordstore.Run{
			RunID: "run-ded", Cluster: "core", Namespace: "test-ns", Cell: "ctrl",
			Tenant: "test-ns/ctrl", JobPath: "app", CreatedAt: base,
		})
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl"), reader, nil)

		rec := callJenkinsLink(t, srv, http.MethodGet, "job/app/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if got, want := linkBody(t, rec), "/pipelines/ctrl/app"; got != want {
			t.Errorf("link = %q, want %q", got, want)
		}
	})

	t.Run("another controller's run with the same job path is not matched", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.addRun(recordstore.Run{
			RunID: "run-other", Cluster: "core", Namespace: "test-ns", Cell: "other-ctrl",
			Tenant: "team-a", JobPath: "team-a/app", BuildNumber: &buildNo, CreatedAt: base,
		})
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), reader, nil)

		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/12/")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (the run belongs to another controller)", rec.Code)
		}
	})

	t.Run("a controller's run older than a page of other runs is found", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.addRun(recordstore.Run{
			RunID: "run-mine", Cluster: "core", Namespace: "test-ns", Cell: "ctrl",
			Tenant: "team-a", JobPath: "team-a/app", CreatedAt: base.Add(-time.Hour),
		})
		for i := 0; i < 55; i++ {
			reader.addRun(recordstore.Run{
				RunID: fmt.Sprintf("run-other-%02d", i), Cluster: "core", Namespace: "test-ns", Cell: "other-ctrl",
				Tenant: "team-a", JobPath: "team-a/app", CreatedAt: base.Add(time.Duration(i) * time.Minute),
			})
		}
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), reader, nil)

		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got, want := linkBody(t, rec), "/pipelines/team-a/team-a%2Fapp"; got != want {
			t.Errorf("link = %q, want %q", got, want)
		}
	})

	t.Run("no matching run answers 404", func(t *testing.T) {
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), newFakeRecordReader(), nil)
		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/12/")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("unreadable run answers 404", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.addRun(recordstore.Run{
			RunID: "run-secret", Cluster: "core", Namespace: "test-ns", Cell: "ctrl",
			Tenant: "team-a", JobPath: "team-a/app", BuildNumber: &buildNo, CreatedAt: base,
		})
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-b"), reader, nil)
		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/12/")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("missing path answers 400", func(t *testing.T) {
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), newFakeRecordReader(), nil)
		rec := callJenkinsLink(t, srv, http.MethodGet, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("nil record store answers 503", func(t *testing.T) {
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), nil, nil)
		rec := callJenkinsLink(t, srv, http.MethodGet, "job/team-a/job/app/12/")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("POST answers 405", func(t *testing.T) {
		srv := pipelinesTestServer(newFakeResourceClient(), pipelineGrantAuthorizer([]string{"read"}, "team-a"), newFakeRecordReader(), nil)
		rec := callJenkinsLink(t, srv, http.MethodPost, "job/team-a/")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}
