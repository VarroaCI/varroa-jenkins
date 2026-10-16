package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// --- 7.5: wiring into handleDesiredState ---

// TestRehydrationRunsBetweenConfigAndItemsThenReloads is the acceptance
// test for the section order D5 sets: config, then rehydration, then
// items, then a reload of every seeded item.
func TestRehydrationRunsBetweenConfigAndItemsThenReloads(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "existing-job")

	var mu sync.Mutex
	var order []string
	rehydratedBeforeItems := false
	firstGet := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/reload":
			mu.Lock()
			order = append(order, "config")
			mu.Unlock()
			http.Redirect(w, r, "/configuration-as-code/", http.StatusFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config.xml"):
			// The items engine's existence check for the new item. At this
			// point the rehydration section, which runs before items, must
			// already have written the seeded build record.
			mu.Lock()
			if firstGet {
				firstGet = false
				if _, err := os.Stat(filepath.Join(jobDir, "builds", "1", "build.xml")); err == nil {
					rehydratedBeforeItems = true
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/createItem"):
			mu.Lock()
			order = append(order, "items")
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == reloadItemRoute:
			mu.Lock()
			order = append(order, "reload")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "items:\n  - kind: folder\n    name: new-item\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/existing-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
			ReloadItems: []string{"new-item"},
		},
	}

	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess {
		t.Fatalf("expected config apply to succeed, got %+v", res)
	}
	if !res.ItemsSuccess {
		t.Fatalf("expected items apply to succeed, got %+v", res)
	}
	if !res.RehydrationSuccess {
		t.Fatalf("expected rehydration to succeed, got %+v", res)
	}
	if !rehydratedBeforeItems {
		t.Error("rehydration did not write its seeded record before the items apply")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"config", "items", "reload"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i, step := range want {
		if order[i] != step {
			t.Errorf("call order[%d] = %q, want %q (full order %v)", i, order[i], step, order)
		}
	}
}

// reloadItemRoute is the plugin route a mite reload posts to.
const reloadItemRoute = "/varroa-run/reload-item"

// reloadItemJobPath returns the job path a reload-item request names.
func reloadItemJobPath(r *http.Request) string {
	var body struct {
		JobPath string `json:"jobPath"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.JobPath
}

// TestRehydrationFailedReloadIsReportedPerItem verifies a failed per-item
// reload fails no section: the rehydration section stays successful, the
// items result stands, and the failure names its item.
func TestRehydrationFailedReloadIsReportedPerItem(t *testing.T) {
	home := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		if strings.HasPrefix(r.URL.Path, "/crumbIssuer") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if r.URL.Path == reloadItemRoute && reloadItemJobPath(r) == "team-a/build" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			ReloadItems: []string{"team-a/build", "team-b/build"},
		},
	}

	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ItemsSuccess {
		t.Errorf("expected the items result to stay successful, got %+v", res)
	}
	if !res.RehydrationSuccess {
		t.Errorf("expected rehydration to stay successful after a failed reload, got %+v", res)
	}
	if res.RehydrationError != "" {
		t.Errorf("RehydrationError = %q, want empty: a reload failure is per item", res.RehydrationError)
	}
	if len(res.RehydrationReloadFailures) != 1 {
		t.Fatalf("RehydrationReloadFailures = %v, want one failure", res.RehydrationReloadFailures)
	}
	failure := res.RehydrationReloadFailures[0]
	if failure.Item != "team-a/build" {
		t.Errorf("failure item = %q, want team-a/build", failure.Item)
	}
	if !strings.Contains(failure.Error, "HTTP 403") {
		t.Errorf("failure error = %q, want the HTTP 403 text", failure.Error)
	}
}

// TestRehydrationReloadFailureKeepsTheSectionOutOfTheMarker verifies a
// failed reload keeps the rehydration section out of the applied marker, so
// a resend reloads the item again without repeating the JCasC reload.
func TestRehydrationReloadFailureKeepsTheSectionOutOfTheMarker(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()

	var mu sync.Mutex
	cascReloads, itemReloads := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/reload":
			mu.Lock()
			cascReloads++
			mu.Unlock()
			http.Redirect(w, r, "/configuration-as-code/", http.StatusFound)
		case r.URL.Path == reloadItemRoute:
			mu.Lock()
			itemReloads++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			ReloadItems: []string{"team-a/build"},
		},
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if len(first.RehydrationReloadFailures) != 1 {
		t.Fatalf("first push: RehydrationReloadFailures = %v, want one failure", first.RehydrationReloadFailures)
	}
	if _, ok := agent.readMarker().sections["rehydration"]; ok {
		t.Error("the marker records the rehydration section after a failed reload")
	}
	mu.Lock()
	reloadsAfterFirst := itemReloads
	mu.Unlock()

	second := agent.handleDesiredState(context.Background(), cmd)
	if len(second.RehydrationReloadFailures) != 1 {
		t.Fatalf("second push: RehydrationReloadFailures = %v, want one failure", second.RehydrationReloadFailures)
	}

	mu.Lock()
	defer mu.Unlock()
	if itemReloads <= reloadsAfterFirst {
		t.Errorf("item reloads = %d after the resend, want more than %d: a resend must reload again", itemReloads, reloadsAfterFirst)
	}
	if cascReloads != 1 {
		t.Errorf("JCasC reloads = %d, want 1: a resend must not repeat the JCasC reload", cascReloads)
	}
}

// TestRehydrationSuccessTracksTheAppliedContentHash proves the mite
// reports rehydration success only for the content_hash it actually
// applied. A first bundle succeeds and stamps the marker with its
// content_hash. A second bundle, same boot session but a different
// content_hash and a path the allow-list rejects, must not be answered
// from the first bundle's cached success: it is genuinely re-applied and
// its own failure is reported. A third apply of the first bundle's
// content_hash then still succeeds, proving the failed second apply left
// the marker's stored hash untouched.
func TestRehydrationSuccessTracksTheAppliedContentHash(t *testing.T) {
	home := t.TempDir()
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	firstCmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		Reload:              true,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/mb/jobs/missing-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
		},
	}
	first := agent.handleDesiredState(context.Background(), firstCmd)
	if !first.RehydrationSuccess {
		t.Fatalf("first apply: expected success, got %+v", first)
	}

	// A different content_hash carrying a rejected path. A mite that
	// answered from the first bundle's cached marker would wrongly
	// report success here.
	secondCmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		Reload:              true,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-2",
			Files: []*mitev1.RehydrationFile{
				{Path: "not-a-jobs-path", GzipContent: nil, RawSize: 0},
			},
		},
	}
	second := agent.handleDesiredState(context.Background(), secondCmd)
	if second.RehydrationSuccess {
		t.Fatalf("second apply: expected failure for a mismatched content_hash with a rejected path, got %+v", second)
	}
	if second.RehydrationError == "" {
		t.Error("second apply: expected a rehydration error for the rejected path")
	}

	// The failed second apply must not have overwritten the marker's
	// stored content-1 hash: the same bundle re-applied still converges
	// to success.
	third := agent.handleDesiredState(context.Background(), firstCmd)
	if !third.RehydrationSuccess {
		t.Fatalf("third apply: expected the original content_hash to still succeed, got %+v", third)
	}
}

// --- 7.6: the marker's fourth section ---

// fakeSessionServer returns a Jenkins stub reporting the given session
// pointer's current value on every response.
func fakeSessionServer(t *testing.T, session *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", *session)
		if strings.HasPrefix(r.URL.Path, "/crumbIssuer") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRehydrationConvergedSkipsSecondApply is the "repeated bundle" case:
// once the marker records a seeded bundle, the same payload inside one
// boot session writes nothing again. The seeded build directory is
// deleted between the two calls; a second write would recreate it.
func TestRehydrationConvergedSkipsSecondApply(t *testing.T) {
	home := t.TempDir()
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	seedJob(t, home, "jobs", "present-job")

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		Reload:              true,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/present-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
		},
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if !first.RehydrationSuccess || len(first.RehydrationDeferredJobs) != 0 {
		t.Fatalf("first apply: expected success with no deferred job, got %+v", first)
	}
	buildDir := filepath.Join(home, "jobs", "present-job", "builds", "1")
	if err := os.RemoveAll(buildDir); err != nil {
		t.Fatal(err)
	}

	second := agent.handleDesiredState(context.Background(), cmd)
	if !second.RehydrationSuccess {
		t.Fatalf("second apply: expected success, got %+v", second)
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Errorf("second apply: converged section must not rewrite the build directory (stat err %v)", err)
	}
}

// TestRehydrationSeedsJobsTheItemsApplyCreates is the new-cell-home case:
// the job does not exist when the rehydration step runs, the items apply
// creates it, and a second pass seeds its build record before the reload.
func TestRehydrationSeedsJobsTheItemsApplyCreates(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	jobDir := filepath.Join(home, "jobs", "new-job")

	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/reload":
			http.Redirect(w, r, "/configuration-as-code/", http.StatusFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config.xml"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/createItem"):
			// Jenkins writes the job directory on create.
			if err := os.MkdirAll(jobDir, 0755); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(filepath.Join(jobDir, "config.xml"), []byte("<flow-definition/>"), 0644); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			order = append(order, "items")
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == reloadItemRoute:
			mu.Lock()
			order = append(order, "reload")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "items:\n  - kind: pipeline\n    name: new-job\n    definition:\n      script: 'pipeline {}'\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/new-job/builds/1/build.xml", []byte("<flow-build/>")),
				rehydrationFile(t, "jobs/new-job/nextBuildNumber", []byte("2")),
			},
			ReloadItems: []string{"new-job"},
		},
	}

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess || !res.RehydrationSuccess {
		t.Fatalf("expected items and rehydration to succeed, got %+v", res)
	}
	if len(res.RehydrationDeferredJobs) != 0 {
		t.Errorf("expected no deferred jobs after the items apply, got %v", res.RehydrationDeferredJobs)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "builds", "1", "build.xml")); err != nil {
		t.Errorf("expected the seeded build record after the items apply: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"items", "reload"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("call order = %v, want %v", order, want)
	}
}

// TestRehydrationDeferredBundleIsNotConverged is the data-loss case: a
// deferred job must keep the rehydration section out of the marker, so
// a resend with the same content hash applies the bundle again.
func TestRehydrationDeferredBundleIsNotConverged(t *testing.T) {
	home := t.TempDir()
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	writeJobConfig(t, home, "mb", multibranchConfig)
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		Reload:              true,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/mb/jobs/missing-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
		},
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if !first.RehydrationSuccess || len(first.RehydrationDeferredJobs) != 1 {
		t.Fatalf("first apply: expected one deferred job and success, got %+v", first)
	}
	markerBytes, err := os.ReadFile(agent.lastAppliedHashPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(markerBytes), "rehydration ") {
		t.Fatal("marker records the rehydration section while a job is deferred")
	}

	seedJob(t, home, "jobs", "mb", "jobs", "missing-job")
	second := agent.handleDesiredState(context.Background(), cmd)
	if !second.RehydrationSuccess {
		t.Fatalf("second apply: expected success, got %+v", second)
	}
	if len(second.RehydrationDeferredJobs) != 0 {
		t.Errorf("second apply: expected no deferred job, got %v", second.RehydrationDeferredJobs)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "mb", "jobs", "missing-job", "builds", "1", "build.xml")); err != nil {
		t.Errorf("second apply: expected the seeded build record: %v", err)
	}
}

// TestRehydrationCompositeSkipNeedsTheRehydrationHash proves the composite
// fast path does not skip a command whose rehydration bundle is not yet
// recorded in the marker, even when the desired-state hash matches.
func TestRehydrationCompositeSkipNeedsTheRehydrationHash(t *testing.T) {
	home := t.TempDir()
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	writeJobConfig(t, home, "mb", multibranchConfig)
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/mb/jobs/late-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
		},
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if len(first.RehydrationDeferredJobs) != 1 {
		t.Fatalf("first apply: expected one deferred job, got %+v", first)
	}

	seedJob(t, home, "jobs", "mb", "jobs", "late-job")
	second := agent.handleDesiredState(context.Background(), cmd)
	if len(second.RehydrationDeferredJobs) != 0 {
		t.Errorf("second apply: expected the composite skip to be bypassed, got deferred %v", second.RehydrationDeferredJobs)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "mb", "jobs", "late-job", "builds", "1", "build.xml")); err != nil {
		t.Errorf("second apply: expected the seeded build record: %v", err)
	}
}

// TestRehydrationBootSessionChangeForcesReapply is the "pod roll"
// scenario: a boot-session change re-applies the section even though the
// bundle's content hash is unchanged.
func TestRehydrationBootSessionChangeForcesReapply(t *testing.T) {
	home := t.TempDir()
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	writeJobConfig(t, home, "mb", multibranchConfig)
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		Reload:              true,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-1",
			Files: []*mitev1.RehydrationFile{
				rehydrationFile(t, "jobs/mb/jobs/missing-job/builds/1/build.xml", []byte("<flow-build/>")),
			},
		},
	}

	first := agent.handleDesiredState(context.Background(), cmd)
	if !first.RehydrationSuccess || len(first.RehydrationDeferredJobs) != 1 {
		t.Fatalf("first apply: expected one deferred job and success, got %+v", first)
	}

	session = "sess-2" // simulates a Jenkins restart between pushes.
	second := agent.handleDesiredState(context.Background(), cmd)
	if !second.RehydrationSuccess {
		t.Fatalf("second apply: expected success, got %+v", second)
	}
	if len(second.RehydrationDeferredJobs) != 1 {
		t.Errorf("second apply: a boot session change should re-apply the section, got %v", second.RehydrationDeferredJobs)
	}
}

// TestRehydrationAbsentPayloadReportsSuccess is the "absent payload" case:
// a command with no rehydration field reports the section successful.
func TestRehydrationAbsentPayloadReportsSuccess(t *testing.T) {
	session := "sess-1"
	srv := fakeSessionServer(t, &session)

	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.RehydrationSuccess {
		t.Errorf("expected rehydration success for a command with no rehydration payload, got %+v", res)
	}
	if res.RehydrationError != "" {
		t.Errorf("expected no rehydration error, got %q", res.RehydrationError)
	}
}

// TestRehydrationAbsentItemIsSkipped asserts an item Jenkins answers 404
// for is skipped: the rehydration section still reports success, so a
// job the tenant removed never fences its other jobs behind NOT_READY.
func TestRehydrationAbsentItemIsSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/reload":
			http.Redirect(w, r, "/configuration-as-code/", http.StatusFound)
		case r.URL.Path == reloadItemRoute:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = t.TempDir()
	res := agent.handleDesiredState(context.Background(), &mitev1.DesiredStateCommand{
		DesiredStateHash:    "h-absent",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration: &mitev1.RehydrationBundle{
			ContentHash: "content-absent",
			ReloadItems: []string{"gone-item"},
		},
	})
	if !res.RehydrationSuccess {
		t.Fatalf("expected rehydration to succeed past an absent item, got %+v", res)
	}
	if res.RehydrationError != "" {
		t.Errorf("expected no rehydration error, got %q", res.RehydrationError)
	}
}
