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
	keep, orphans := orphanDeferredJobs(home, deferred, nil, nil)

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
	return rehydrationServerWithBranches(t, home, created, fail, "")
}

// rehydrationServerWithBranches is rehydrationServer, and it also answers
// the branch inventory with branchesJSON when that is not empty.
func rehydrationServerWithBranches(t *testing.T, home string, created []string, fail bool, branchesJSON string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == jobBranchesPath && branchesJSON != "":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, branchesJSON)
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
	}, nil, nil)

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

// branchSet builds an inventory from rows.
func branchSet(rows ...*mitev1.JobBranchProject) *mitev1.JobBranchSet {
	return &mitev1.JobBranchSet{Items: rows}
}

// branchRow builds a project row with the given index result and heads.
func branchRow(jobPath, indexResult string, heads ...string) *mitev1.JobBranchProject {
	row := &mitev1.JobBranchProject{JobPath: jobPath, IndexResult: indexResult}
	for _, h := range heads {
		row.Heads = append(row.Heads, &mitev1.JobBranchHead{Name: h, JobPath: h, Kind: "branch"})
	}
	return row
}

// writeOrgLayout writes an organization folder at sbsoftware/repos. Its
// repository directory is absent.
func writeOrgLayout(t *testing.T, home string) {
	t.Helper()
	writeJobConfig(t, home, "sbsoftware", folderConfig)
	writeJobConfig(t, home, "sbsoftware/repos", orgFolderConfig)
}

func TestOrphanDeferredJobsBranchInventory(t *testing.T) {
	const success = branchIndexSucceeded
	mbSetup := func(t *testing.T, home string) { writeJobConfig(t, home, "mb", multibranchConfig) }
	tests := []struct {
		name       string
		setup      func(t *testing.T, home string)
		job        string
		set        *mitev1.JobBranchSet
		wantOrphan bool
	}{
		{
			name:       "multibranch success, head absent orphans",
			setup:      mbSetup,
			job:        "mb/main",
			set:        branchSet(branchRow("mb", success)),
			wantOrphan: true,
		},
		{
			name:  "multibranch success, head present keeps",
			setup: mbSetup,
			job:   "mb/main",
			set:   branchSet(branchRow("mb", success, "mb/main")),
		},
		{
			name:  "multibranch scan still running keeps",
			setup: mbSetup,
			job:   "mb/main",
			set:   branchSet(branchRow("mb", "")),
		},
		{
			name:  "multibranch failed scan keeps",
			setup: mbSetup,
			job:   "mb/main",
			set:   branchSet(branchRow("mb", "FAILURE")),
		},
		{
			name:  "multibranch truncated heads keeps",
			setup: mbSetup,
			job:   "mb/main",
			set: branchSet(&mitev1.JobBranchProject{
				JobPath: "mb", IndexResult: success, HeadsTruncated: true,
			}),
		},
		{
			name:  "multibranch without a row keeps",
			setup: mbSetup,
			job:   "mb/main",
			set:   branchSet(branchRow("other", success)),
		},
		{
			name:  "nil inventory keeps",
			setup: mbSetup,
			job:   "mb/main",
			set:   nil,
		},
		{
			name:       "organization folder success, repository absent orphans",
			setup:      writeOrgLayout,
			job:        "sbsoftware/repos/smoke-push/main",
			set:        branchSet(branchRow("sbsoftware/repos", success)),
			wantOrphan: true,
		},
		{
			name:       "organization folder, repository scan success, branch absent orphans",
			setup:      writeOrgLayout,
			job:        "sbsoftware/repos/smoke-push/main",
			set:        branchSet(branchRow("sbsoftware/repos", success, "sbsoftware/repos/smoke-push"), branchRow("sbsoftware/repos/smoke-push", success)),
			wantOrphan: true,
		},
		{
			name:  "organization folder, repository scan running keeps",
			setup: writeOrgLayout,
			job:   "sbsoftware/repos/smoke-push/main",
			set:   branchSet(branchRow("sbsoftware/repos", success, "sbsoftware/repos/smoke-push"), branchRow("sbsoftware/repos/smoke-push", "")),
		},
		{
			name:  "organization folder, repository without a row keeps",
			setup: writeOrgLayout,
			job:   "sbsoftware/repos/smoke-push/main",
			set:   branchSet(branchRow("sbsoftware/repos", success, "sbsoftware/repos/smoke-push")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.setup(t, home)
			keep, orphans := orphanDeferredJobs(home, []string{tt.job}, nil, func() *mitev1.JobBranchSet { return tt.set })
			if tt.wantOrphan {
				if want := []string{tt.job}; !reflect.DeepEqual(orphans, want) || len(keep) != 0 {
					t.Errorf("keep = %v, orphans = %v; want orphan %v", keep, orphans, want)
				}
				return
			}
			if want := []string{tt.job}; !reflect.DeepEqual(keep, want) || len(orphans) != 0 {
				t.Errorf("keep = %v, orphans = %v; want keep %v", keep, orphans, want)
			}
		})
	}
}

func TestOrphanDeferredJobsLoadsInventoryOnce(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	writeJobConfig(t, home, "tenant", folderConfig)
	if err := os.MkdirAll(filepath.Join(home, "jobs", "bare"), 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		deferred []string
		want     int
	}{
		{"two multibranch jobs load once", []string{"mb/a", "mb/b"}, 1},
		{"plain folder and no-config jobs never load", []string{"tenant/gone", "bare/child", "missing/child"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			orphanDeferredJobs(home, tt.deferred, nil, func() *mitev1.JobBranchSet {
				calls++
				return branchSet()
			})
			if calls != tt.want {
				t.Errorf("loader calls = %d, want %d", calls, tt.want)
			}
		})
	}
}

// TestRehydrationDropsBranchDeletedAfterScan runs the whole desired-state
// apply. The multibranch scan lists mb/main only, so the records of mb/gone
// drop, and mb/main stays deferred until its branch loads.
func TestRehydrationDropsBranchDeletedAfterScan(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	const branches = `{"projects":[{"jobPath":"mb","indexResult":"SUCCESS","heads":[{"name":"main","jobPath":"mb/main","kind":"branch"}]}],"total":1}`
	srv := rehydrationServerWithBranches(t, home, []string{"new-job"}, false, branches)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-scan", &mitev1.RehydrationBundle{
		ContentHash: "content-scan",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/mb/jobs/main/nextBuildNumber", []byte("2")),
			rehydrationFile(t, "jobs/mb/jobs/gone/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess || !res.RehydrationSuccess {
		t.Fatalf("expected items and rehydration to succeed, got %+v", res)
	}
	if want := []string{"mb/main"}; !reflect.DeepEqual(res.RehydrationDeferredJobs, want) {
		t.Errorf("RehydrationDeferredJobs = %v, want %v", res.RehydrationDeferredJobs, want)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "mb", "jobs", "gone")); !os.IsNotExist(err) {
		t.Errorf("expected no directory for the deleted branch, stat err = %v", err)
	}
}

func TestBranchScanFailure(t *testing.T) {
	const failure = "FAILURE"
	tests := []struct {
		name       string
		set        *mitev1.JobBranchSet
		folder     string
		job        string
		wantFolder string
		wantResult string
	}{
		{
			name:       "multibranch failed scan",
			set:        branchSet(branchRow("mb", failure)),
			folder:     "mb",
			job:        "mb/main",
			wantFolder: "mb",
			wantResult: failure,
		},
		{
			name:       "multibranch aborted scan",
			set:        branchSet(branchRow("mb", "ABORTED")),
			folder:     "mb",
			job:        "mb/main",
			wantFolder: "mb",
			wantResult: "ABORTED",
		},
		{
			name:   "multibranch successful scan",
			set:    branchSet(branchRow("mb", branchIndexSucceeded)),
			folder: "mb",
			job:    "mb/main",
		},
		{
			name:   "multibranch scan still running",
			set:    branchSet(branchRow("mb", "")),
			folder: "mb",
			job:    "mb/main",
		},
		{
			name:   "no row",
			set:    branchSet(),
			folder: "mb",
			job:    "mb/main",
		},
		{
			name:   "nil set",
			set:    nil,
			folder: "mb",
			job:    "mb/main",
		},
		{
			name: "organization folder lists the repository, repository failed",
			set: branchSet(
				branchRow("sbsoftware/repos", branchIndexSucceeded, "sbsoftware/repos/smoke-push"),
				branchRow("sbsoftware/repos/smoke-push", failure),
			),
			folder:     "sbsoftware/repos",
			job:        "sbsoftware/repos/smoke-push/main",
			wantFolder: "sbsoftware/repos/smoke-push",
			wantResult: failure,
		},
		{
			name:       "organization folder itself failed",
			set:        branchSet(branchRow("sbsoftware/repos", failure)),
			folder:     "sbsoftware/repos",
			job:        "sbsoftware/repos/smoke-push/main",
			wantFolder: "sbsoftware/repos",
			wantResult: failure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFolder, gotResult := branchScanFailure(tt.set, tt.folder, tt.job)
			if gotFolder != tt.wantFolder || gotResult != tt.wantResult {
				t.Errorf("branchScanFailure = (%q, %q), want (%q, %q)",
					gotFolder, gotResult, tt.wantFolder, tt.wantResult)
			}
		})
	}
}

func TestFailedBranchScansGroupsByFolder(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	writeJobConfig(t, home, "plain", folderConfig)
	if err := os.MkdirAll(filepath.Join(home, "jobs", "bare"), 0755); err != nil {
		t.Fatal(err)
	}
	set := branchSet(branchRow("mb", "FAILURE"))
	keep := []string{"mb/main", "mb/feature", "plain/child", "bare/main"}

	got := failedBranchScans(home, keep, set)
	want := []*mitev1.RehydrationFailedScan{{Folder: "mb", IndexResult: "FAILURE", DeferredJobs: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("failedBranchScans = %v, want %v", got, want)
	}
}

func TestRehydrationReportsFailedBranchScan(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	const branches = `{"projects":[{"jobPath":"mb","indexResult":"FAILURE","heads":[]}],"total":1}`
	srv := rehydrationServerWithBranches(t, home, []string{"new-job"}, false, branches)

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-failed-scan", &mitev1.RehydrationBundle{
		ContentHash: "content-failed-scan",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/mb/jobs/main/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.ItemsSuccess || !res.RehydrationSuccess {
		t.Fatalf("expected items and rehydration to succeed, got %+v", res)
	}
	if want := []string{"mb/main"}; !reflect.DeepEqual(res.RehydrationDeferredJobs, want) {
		t.Errorf("RehydrationDeferredJobs = %v, want %v", res.RehydrationDeferredJobs, want)
	}
	want := []*mitev1.RehydrationFailedScan{{Folder: "mb", IndexResult: "FAILURE", DeferredJobs: 1}}
	if !reflect.DeepEqual(res.RehydrationFailedScans, want) {
		t.Errorf("RehydrationFailedScans = %v, want %v", res.RehydrationFailedScans, want)
	}
	if res.RehydrationBranchScansUnknown {
		t.Error("RehydrationBranchScansUnknown = true, want false")
	}
}

func TestRehydrationBranchScansUnknownOnPollFailure(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	// An empty branch response fails to decode, so the poll reports an error.
	srv := rehydrationServerWithBranches(t, home, []string{"new-job"}, false, "")

	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.jenkinsHomeDir = home
	cmd := orphanCommand("hash-poll-failure", &mitev1.RehydrationBundle{
		ContentHash: "content-poll-failure",
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/mb/jobs/main/nextBuildNumber", []byte("2")),
		},
	})

	res := agent.handleDesiredState(context.Background(), cmd)
	if !res.RehydrationBranchScansUnknown {
		t.Error("RehydrationBranchScansUnknown = false, want true")
	}
	if len(res.RehydrationFailedScans) != 0 {
		t.Errorf("RehydrationFailedScans = %v, want none", res.RehydrationFailedScans)
	}
}
