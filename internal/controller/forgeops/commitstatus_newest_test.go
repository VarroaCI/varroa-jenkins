package forgeops

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const (
	// statusRoot is the Jenkins location URL of the cell: the dashboard door.
	statusRoot = "https://varroa.example.org/headless/core/cells/cell-a/"
	// statusOlderURL and statusNewestURL are the build URLs of builds 7 and 8.
	statusOlderURL  = statusRoot + "job/payments/job/app/job/main/7/display/redirect"
	statusNewestURL = statusRoot + "job/payments/job/app/job/main/8/display/redirect"
	// giteaHeadContext is the context that the gitea plugin writes for a
	// branch build of payments/app/main.
	giteaHeadContext = "payments/app/pipeline/head"
)

// newestReq returns a request that puts the result of build 8 back over
// build 7.
func newestReq(result string) forgeapi.NewestStatusRequest {
	return forgeapi.NewestStatusRequest{
		Tenant:           "payments",
		Repo:             testRepoURL,
		SHA:              testSHA,
		JobPath:          "payments/app/main",
		BuildNumber:      8,
		Result:           result,
		SupersededBuilds: []int64{7},
		JenkinsURL:       statusRoot,
	}
}

// TestRestoreNewestStatus_WritesOverSupersededBuild checks that a default
// context whose latest status names the older build gets the newest build's
// state, link, and description, whatever the older state is.
func TestRestoreNewestStatus_WritesOverSupersededBuild(t *testing.T) {
	for _, olderState := range []string{forge.StateSuccess, forge.StateFailure, forge.StateError, forge.StatePending} {
		t.Run(olderState, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			c := pushable()
			c.statuses = []forge.CommitStatus{
				{Context: giteaHeadContext, State: olderState, TargetURL: statusOlderURL, Description: "older"},
			}
			oneWriter(fx, c)
			n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("FAILURE"))
			if err != nil || n != 1 {
				t.Fatalf("written %d, err %v; want 1, nil", n, err)
			}
			w := c.statusWrites[0]
			if w.Context != giteaHeadContext || w.State != forge.StateFailure || w.TargetURL != statusNewestURL {
				t.Fatalf("write = %+v", w)
			}
			if w.Description != "Varroa kept the result of the newest build #8: FAILURE" {
				t.Fatalf("description = %q", w.Description)
			}
			if c.statusWriteSHAs[0] != testSHA {
				t.Fatalf("sha = %q, want %q", c.statusWriteSHAs[0], testSHA)
			}
		})
	}
}

// TestRestoreNewestStatus_WritesEachDefaultContext checks the default
// contexts of each forge kind.
func TestRestoreNewestStatus_WritesEachDefaultContext(t *testing.T) {
	cases := map[forge.Kind][]string{
		forge.KindGitHub: {
			"continuous-integration/jenkins/branch",
			"continuous-integration/jenkins/pr-merge",
			"continuous-integration/jenkins/pr-head",
		},
		forge.KindGitea: {
			"payments/app/pipeline/head",
			"payments/app/pipeline/pr-main",
			"payments/app/pipeline/tag",
			"payments/app/pipeline/release",
		},
	}
	for kind, contexts := range cases {
		for _, name := range contexts {
			t.Run(string(kind)+" "+name, func(t *testing.T) {
				fx := newFixture(t, "agents-payments")
				fx.kind = kind
				c := pushable()
				c.statuses = []forge.CommitStatus{{Context: name, State: forge.StateFailure, TargetURL: statusOlderURL}}
				oneWriter(fx, c)
				n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("SUCCESS"))
				if err != nil || n != 1 || c.statusWrites[0].Context != name {
					t.Fatalf("written %d, writes %+v, err %v; want one write of %q", n, c.statusWrites, err, name)
				}
			})
		}
	}
}

// TestRestoreNewestStatus_LeavesOtherContexts checks that a context is never
// written when its latest status names the newest build, another job, another
// build, only the job, or another Jenkins, or when no branch-source plugin
// writes that context by default.
func TestRestoreNewestStatus_LeavesOtherContexts(t *testing.T) {
	cases := map[forge.Kind][]forge.CommitStatus{
		forge.KindGitea: {
			{Context: giteaHeadContext, State: forge.StateSuccess, TargetURL: statusNewestURL},
			{Context: "payments/app/pipeline/pr-", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: "payments/other/pipeline/head", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: "ci/deploy", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: "continuous-integration/jenkins/branch", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: giteaHeadContext, State: forge.StateFailure, TargetURL: statusRoot + "job/payments/job/app/job/dev/7/display/redirect"},
			{Context: giteaHeadContext, State: forge.StateFailure, TargetURL: statusRoot + "job/payments/job/app/job/main/9/display/redirect"},
			{Context: giteaHeadContext, State: forge.StatePending, TargetURL: statusRoot + "job/payments/job/app/job/main/display/redirect"},
			{Context: giteaHeadContext, State: forge.StateFailure, TargetURL: "https://other-jenkins/job/payments/job/app/job/main/7/"},
			{Context: giteaHeadContext, State: forge.StateFailure},
		},
		forge.KindGitHub: {
			{Context: "continuous-integration/jenkins/pr-merge/deploy", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: "Jenkins", State: forge.StateFailure, TargetURL: statusOlderURL},
			{Context: giteaHeadContext, State: forge.StateFailure, TargetURL: statusOlderURL},
		},
	}
	for kind, statuses := range cases {
		t.Run(string(kind), func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			fx.kind = kind
			c := pushable()
			c.statuses = statuses
			oneWriter(fx, c)
			n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("SUCCESS"))
			if err != nil || n != 0 || len(c.statusWrites) != 0 {
				t.Fatalf("written %d, writes %+v, err %v; want none", n, c.statusWrites, err)
			}
		})
	}
}

// TestRestoreNewestStatus_RefusesIncompleteRequest checks that a request
// without its facts reaches no forge.
func TestRestoreNewestStatus_RefusesIncompleteRequest(t *testing.T) {
	cases := map[string]func(*forgeapi.NewestStatusRequest){
		"no tenant":      func(r *forgeapi.NewestStatusRequest) { r.Tenant = "" },
		"no repo":        func(r *forgeapi.NewestStatusRequest) { r.Repo = "" },
		"no sha":         func(r *forgeapi.NewestStatusRequest) { r.SHA = "" },
		"no job":         func(r *forgeapi.NewestStatusRequest) { r.JobPath = "" },
		"no result":      func(r *forgeapi.NewestStatusRequest) { r.Result = "" },
		"no build":       func(r *forgeapi.NewestStatusRequest) { r.BuildNumber = 0 },
		"no superseded":  func(r *forgeapi.NewestStatusRequest) { r.SupersededBuilds = nil },
		"negative build": func(r *forgeapi.NewestStatusRequest) { r.BuildNumber = -1 },
		"no jenkins url": func(r *forgeapi.NewestStatusRequest) { r.JenkinsURL = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			noForgeCall(fx)
			req := newestReq("SUCCESS")
			mutate(&req)
			_, err := fx.ops.RestoreNewestStatus(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), forgeapi.CodeInvalidRequest) {
				t.Fatalf("err = %v, want %s", err, forgeapi.CodeInvalidRequest)
			}
		})
	}
}

// TestRestoreNewestStatus_ForbiddenWriteFallsBackToNextPusher checks that a
// 403 on the write moves the restore to the next pushing credential.
func TestRestoreNewestStatus_ForbiddenWriteFallsBackToNextPusher(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	older := []forge.CommitStatus{{Context: giteaHeadContext, State: forge.StateSuccess, TargetURL: statusOlderURL}}
	a := pushable()
	a.statuses = older
	a.setStatusErr = forge.ErrForbidden
	b := pushable()
	b.statuses = older
	twoCredentials(fx, a, b)
	n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("FAILURE"))
	if err != nil || n != 1 {
		t.Fatalf("written %d, err %v; want 1, nil", n, err)
	}
	if len(b.statusWrites) != 1 || b.statusWrites[0].TargetURL != statusNewestURL {
		t.Fatalf("writes of b = %+v", b.statusWrites)
	}
}

// TestReplaceBuildNumber covers the target URL forms that a build status
// carries, under the Jenkins location URL of the cell.
func TestReplaceBuildNumber(t *testing.T) {
	const inCluster = "http://abc-svc.cells.svc.cluster.local:8080/"
	cases := []struct {
		name   string
		target string
		root   string
		job    string
		want   string
		ok     bool
	}{
		{"dashboard door with display redirect", statusOlderURL, statusRoot, "payments/app/main", statusNewestURL, true},
		{
			"dashboard door with a trailing slash",
			statusRoot + "job/payments/job/app/job/main/7/",
			statusRoot, "payments/app/main",
			statusRoot + "job/payments/job/app/job/main/8/",
			true,
		},
		{
			"display redirect with a trailing slash",
			statusRoot + "job/payments/job/app/job/main/7/display/redirect/",
			statusRoot, "payments/app/main",
			statusRoot + "job/payments/job/app/job/main/8/display/redirect/",
			true,
		},
		{
			"root without a trailing slash",
			statusOlderURL,
			strings.TrimSuffix(statusRoot, "/"), "payments/app/main",
			statusNewestURL,
			true,
		},
		{
			"host in another case",
			"https://VARROA.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/7/display/redirect",
			statusRoot, "payments/app/main",
			"https://VARROA.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/8/display/redirect",
			true,
		},
		{
			"in-cluster endpoint",
			inCluster + "job/payments/job/app/job/main/7/display/redirect",
			inCluster, "payments/app/main",
			inCluster + "job/payments/job/app/job/main/8/display/redirect",
			true,
		},
		{
			"encoded branch slash",
			statusRoot + "job/payments/job/app/job/feature%252Fx/7/display/redirect",
			statusRoot, "payments/app/feature%2Fx",
			statusRoot + "job/payments/job/app/job/feature%252Fx/8/display/redirect",
			true,
		},
		{
			"query kept",
			statusOlderURL + "?utm_source=forge",
			statusRoot, "payments/app/main",
			statusNewestURL + "?utm_source=forge",
			true,
		},
		{"another host", "https://other-jenkins/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"another host with the root path", "https://other.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"another scheme", "http://varroa.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"another port", "https://varroa.example.org:8443/headless/core/cells/cell-a/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"another cell", "https://varroa.example.org/headless/core/cells/cell-b/job/payments/job/app/job/main/7/display/redirect", statusRoot, "payments/app/main", "", false},
		{"another cluster", "https://varroa.example.org/headless/member-b/cells/cell-a/job/payments/job/app/job/main/7/display/redirect", statusRoot, "payments/app/main", "", false},
		{"no root path", "https://varroa.example.org/job/payments/job/app/job/main/7/display/redirect", statusRoot, "payments/app/main", "", false},
		{"a folder between the root and the job", statusRoot + "job/other/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"build 70 is not build 7", statusRoot + "job/payments/job/app/job/main/70/", statusRoot, "payments/app/main", "", false},
		{"a page under the build", statusRoot + "job/payments/job/app/job/main/7/console", statusRoot, "payments/app/main", "", false},
		{"job url", statusRoot + "job/payments/job/app/job/main/display/redirect", statusRoot, "payments/app/main", "", false},
		{"another job", statusRoot + "job/payments/job/app/job/dev/7/", statusRoot, "payments/app/main", "", false},
		{"user info", "https://someone@varroa.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"url that does not parse", "https://[::1/job/payments/job/app/job/main/7/", statusRoot, "payments/app/main", "", false},
		{"empty target", "", statusRoot, "payments/app/main", "", false},
		{"empty root", statusOlderURL, "", "payments/app/main", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := replaceBuildNumber(tc.target, tc.root, tc.job, 7, 8)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("replaceBuildNumber(%q) = %q, %v; want %q, %v", tc.target, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestBranchSourceContext covers the default context names of each forge
// kind for a job and a job of another repository.
func TestBranchSourceContext(t *testing.T) {
	cases := []struct {
		kind forge.Kind
		name string
		job  string
		want bool
	}{
		{forge.KindGitHub, "continuous-integration/jenkins/branch", "payments/app/main", true},
		{forge.KindGitHub, "continuous-integration/jenkins/pr-merge", "payments/app/PR-4", true},
		{forge.KindGitHub, "continuous-integration/jenkins/pr-head", "payments/app/PR-4", true},
		{forge.KindGitHub, "continuous-integration/jenkins/tag", "payments/app/v1", false},
		{forge.KindGitHub, "ci/build", "payments/app/main", false},
		{forge.KindGitea, "payments/app/pipeline/head", "payments/app/main", true},
		{forge.KindGitea, "payments/app/pipeline/pr-main", "payments/app/PR-4", true},
		{forge.KindGitea, "payments/app/pipeline/tag", "payments/app/v1", true},
		{forge.KindGitea, "payments/app/pipeline/release", "payments/app/v1", true},
		{forge.KindGitea, "payments/app/pipeline/pr-", "payments/app/PR-4", false},
		{forge.KindGitea, "payments/app/pipeline/deploy", "payments/app/main", false},
		{forge.KindGitea, "payments/other/pipeline/head", "payments/app/main", false},
		{forge.KindGitea, "app/pipeline/head", "app", false},
		{forge.KindGitea, "continuous-integration/jenkins/branch", "payments/app/main", false},
		{forge.Kind("bitbucket"), "continuous-integration/jenkins/branch", "payments/app/main", false},
	}
	for _, tc := range cases {
		if got := branchSourceContext(tc.kind, tc.name, tc.job); got != tc.want {
			t.Errorf("branchSourceContext(%s, %q, %q) = %v, want %v", tc.kind, tc.name, tc.job, got, tc.want)
		}
	}
}
