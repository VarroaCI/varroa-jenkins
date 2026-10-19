package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	folderConfig      = `<com.cloudbees.hudson.plugins.folder.Folder plugin="cloudbees-folder"/>`
	multibranchConfig = `<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject plugin="workflow-multibranch"/>`
	orgFolderConfig   = `<jenkins.branch.OrganizationFolder plugin="branch-api"/>`
	// jenkinsXMLProlog is the declaration Jenkins writes on every item.
	jenkinsXMLProlog = "<?xml version='1.1' encoding='UTF-8'?>\n"
)

// writeJobConfig writes a config.xml under the job directory of jobPath.
func writeJobConfig(t *testing.T, home, jobPath, config string) {
	t.Helper()
	parsed, err := parseRehydrationPath("jobs/" + strings.ReplaceAll(jobPath, "/", "/jobs/") + "/nextBuildNumber")
	if err != nil {
		t.Fatalf("parse %q: %v", jobPath, err)
	}
	dir := filepath.Join(append([]string{home}, parsed.jobDirParts...)...)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte(jenkinsXMLProlog+config), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanDeferredJobs(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "tenant", folderConfig)
	writeJobConfig(t, home, "tenant/mb", multibranchConfig)
	writeJobConfig(t, home, "org", orgFolderConfig)
	if err := os.MkdirAll(filepath.Join(home, "jobs", "bare", "jobs"), 0755); err != nil {
		t.Fatal(err)
	}

	deferred := []string{
		"tenant-x",               // top level, absent
		"tenant/gone",            // plain folder parent
		"tenant/mb/feature",      // multibranch parent
		"org/repo",               // organization folder parent
		"missing/child",          // parent directory absent
		"bare/child",             // parent directory exists, no config.xml yet
		"org/repos/projgen/main", // organization folder grandparent, parent absent
		"tenant/nest/one/job",    // plain folder grandparent, parent absent
	}
	keep, orphans := orphanDeferredJobs(home, deferred)

	wantKeep := []string{"tenant/mb/feature", "org/repo", "bare/child", "org/repos/projgen/main"}
	wantOrphans := []string{"tenant-x", "tenant/gone", "missing/child", "tenant/nest/one/job"}
	if !reflect.DeepEqual(keep, wantKeep) {
		t.Errorf("keep = %v, want %v", keep, wantKeep)
	}
	if !reflect.DeepEqual(orphans, wantOrphans) {
		t.Errorf("orphans = %v, want %v", orphans, wantOrphans)
	}
}

// rehydrationServer answers the Jenkins calls of the items apply. createItem
// writes the config of each name in created, or fails when fail is set.
func rehydrationServer(t *testing.T, home string, created []string, fail bool) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
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
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, name := range created {
				dir := filepath.Join(home, "jobs", name)
				if err := os.MkdirAll(dir, 0755); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<flow-definition/>"), 0644); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const newJobItems = "items:\n  - kind: pipeline\n    name: new-job\n    definition:\n      script: 'pipeline {}'\n"

func orphanCommand(hash string, bundle *mitev1.RehydrationBundle) *mitev1.DesiredStateCommand {
	return &mitev1.DesiredStateCommand{
		DesiredStateHash:    hash,
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           newJobItems,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
		Rehydration:         bundle,
	}
}

func TestRehydrationDropsRecordsOfDeletedJobs(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-orphan", &mitev1.RehydrationBundle{
		ContentHash: "content-orphan",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/new-job/nextBuildNumber", []byte("2")),
			rehydrationFile(t, "jobs/deleted-job/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess || !res.RehydrationSuccess {
		t.Fatalf("expected items and rehydration to succeed, got %+v", res)
	}
	if len(res.RehydrationDeferredJobs) != 0 {
		t.Errorf("expected the deleted job dropped, not deferred; got %v", res.RehydrationDeferredJobs)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "deleted-job")); !os.IsNotExist(err) {
		t.Errorf("expected no directory for the deleted job, stat err = %v", err)
	}
}

func TestRehydrationKeepsMultibranchChildDeferred(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-mb", &mitev1.RehydrationBundle{
		ContentHash: "content-mb",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/mb/jobs/feature/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess {
		t.Fatalf("expected items to succeed, got %+v", res)
	}
	if want := []string{"mb/feature"}; !reflect.DeepEqual(res.RehydrationDeferredJobs, want) {
		t.Errorf("RehydrationDeferredJobs = %v, want %v", res.RehydrationDeferredJobs, want)
	}
}

func TestRehydrationFailedItemsKeepsEveryDeferral(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	srv := rehydrationServer(t, home, nil, true)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-fail", &mitev1.RehydrationBundle{
		ContentHash: "content-fail",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/deleted-job/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if res.ItemsSuccess {
		t.Fatalf("expected the items apply to fail, got %+v", res)
	}
	if want := []string{"deleted-job"}; !reflect.DeepEqual(res.RehydrationDeferredJobs, want) {
		t.Errorf("RehydrationDeferredJobs = %v, want %v", res.RehydrationDeferredJobs, want)
	}
}

// TestOrphanDeferredJobsOrganizationFolderLayout models the cell layout that
// follows a restart: the organization folder config exists, and its repository
// directory does not exist until the folder's first scan creates it. The
// deferred branch job stays deferred. A job under a plain folder still orphans.
func TestOrphanDeferredJobsOrganizationFolderLayout(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "sbsoftware", folderConfig)
	writeJobConfig(t, home, "sbsoftware/repos", orgFolderConfig)

	keep, orphans := orphanDeferredJobs(home, []string{
		"sbsoftware/repos/projgen/main",
		"sbsoftware/projgen/main",
	})

	if want := []string{"sbsoftware/repos/projgen/main"}; !reflect.DeepEqual(keep, want) {
		t.Errorf("keep = %v, want %v", keep, want)
	}
	if want := []string{"sbsoftware/projgen/main"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
}

// TestRehydrationSeedsIndexedBranchJob runs a branch job's seed through the
// whole desired-state apply. The branch directory exists before the apply,
// so the seed lands there and no deferral is reported.
func TestRehydrationSeedsIndexedBranchJob(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	writeConfigAt(t, filepath.Join(home, "jobs", "mb", "branches", "main"), fmt.Sprintf(branchJobConfig, "main"))
	srv := rehydrationServer(t, home, []string{"new-job"}, false)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-branch", &mitev1.RehydrationBundle{
		ContentHash: "content-branch",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/mb/jobs/main/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess || !res.RehydrationSuccess {
		t.Fatalf("expected items and rehydration to succeed, got %+v", res)
	}
	if len(res.RehydrationDeferredJobs) != 0 {
		t.Errorf("RehydrationDeferredJobs = %v, want none", res.RehydrationDeferredJobs)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "mb", "branches", "main", "nextBuildNumber")); err != nil {
		t.Errorf("nextBuildNumber not written under branches/main: %v", err)
	}
}
