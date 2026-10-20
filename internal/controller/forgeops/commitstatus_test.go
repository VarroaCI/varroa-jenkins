package forgeops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const (
	statusBuildURL = "https://jenkins.example.org/job/payments/job/app/job/main/7/"
	statusOtherURL = "https://jenkins.example.org/job/payments/job/app/job/main/8/"
)

func endReq() forgeapi.CommitStatusRequest {
	return forgeapi.CommitStatusRequest{
		Team:        "payments",
		Repo:        testRepoURL,
		SHA:         testSHA,
		JobPath:     "payments/app/main",
		BuildNumber: 7,
		Reason:      "cell-lost",
	}
}

// TestEndPendingStatus_OverwritesPendingOfTheBuild checks that the pending
// context of this build is set to error and keeps its name and target URL.
func TestEndPendingStatus_OverwritesPendingOfTheBuild(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusBuildURL, Description: "running"},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 1 {
		t.Fatalf("written %d, err %v; want 1, nil", n, err)
	}
	if len(c.statusWrites) != 1 {
		t.Fatalf("writes = %d, want 1", len(c.statusWrites))
	}
	w := c.statusWrites[0]
	if w.Context != "ci/jenkins" || w.TargetURL != statusBuildURL || w.State != forge.StateError {
		t.Fatalf("write = %+v", w)
	}
	if w.Description != "Varroa ended the build: cell-lost" {
		t.Fatalf("description = %q", w.Description)
	}
	if c.statusWriteSHAs[0] != testSHA {
		t.Fatalf("sha = %q, want %q", c.statusWriteSHAs[0], testSHA)
	}
}

// TestEndPendingStatus_SkipsContextWithEndState checks that a context whose
// latest state is an end state is not written.
func TestEndPendingStatus_SkipsContextWithEndState(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: "success", TargetURL: statusBuildURL},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 0 || len(c.statusWrites) != 0 {
		t.Fatalf("written %d, writes %v, err %v; want none", n, c.statusWrites, err)
	}
}

// TestEndPendingStatus_SkipsOtherBuildNumber checks the automatic retry case:
// the pending status of build 8 is not written when build 7 ends.
func TestEndPendingStatus_SkipsOtherBuildNumber(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusOtherURL},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 0 || len(c.statusWrites) != 0 {
		t.Fatalf("written %d, writes %v, err %v; want none", n, c.statusWrites, err)
	}
}

// TestEndPendingStatus_NoStatusesWritesNothing checks a commit without any
// status. The call succeeds and writes nothing.
func TestEndPendingStatus_NoStatusesWritesNothing(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 0 || len(c.statusReads) != 1 {
		t.Fatalf("written %d, reads %v, err %v", n, c.statusReads, err)
	}
}

// TestEndPendingStatus_NoMatchingCredential checks a Team without a credential
// for the host. The call refuses and never reaches the forge.
func TestEndPendingStatus_NoMatchingCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	_, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err == nil || !strings.Contains(err.Error(), forgeapi.CodeNoMatchingCredential) {
		t.Fatalf("err = %v, want %s", err, forgeapi.CodeNoMatchingCredential)
	}
}

// TestEndPendingStatus_InvalidRequest checks the field rules.
func TestEndPendingStatus_InvalidRequest(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	cases := map[string]func(*forgeapi.CommitStatusRequest){
		"no team":        func(r *forgeapi.CommitStatusRequest) { r.Team = "" },
		"no sha":         func(r *forgeapi.CommitStatusRequest) { r.SHA = "" },
		"no job":         func(r *forgeapi.CommitStatusRequest) { r.JobPath = "" },
		"no reason":      func(r *forgeapi.CommitStatusRequest) { r.Reason = "" },
		"zero build":     func(r *forgeapi.CommitStatusRequest) { r.BuildNumber = 0 },
		"negative build": func(r *forgeapi.CommitStatusRequest) { r.BuildNumber = -1 },
		"bad repo url":   func(r *forgeapi.CommitStatusRequest) { r.Repo = "git@[bad" },
		"no repo at all": func(r *forgeapi.CommitStatusRequest) { r.Repo = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := endReq()
			mutate(&req)
			if _, err := fx.ops.EndPendingStatus(context.Background(), req); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// TestEndPendingStatus_ForgeWriteFails checks that a failed write returns the
// error with the count of the writes that succeeded.
func TestEndPendingStatus_ForgeWriteFails(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{{Context: "ci", State: forge.StatePending, TargetURL: statusBuildURL}}
	c.setStatusErr = errors.New("boom")
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err == nil || n != 0 {
		t.Fatalf("written %d, err %v; want 0 and an error", n, err)
	}
}

// TestBuildURLMatches covers the URL forms that a branch-source plugin writes.
func TestBuildURLMatches(t *testing.T) {
	cases := []struct {
		name   string
		target string
		job    string
		number int64
		want   bool
	}{
		{"plain build", "https://j.example.org/job/payments/job/app/job/main/7/", "payments/app/main", 7, true},
		{"headless redirect", "https://j.example.org/job/payments/job/app/job/main/7/display/redirect", "payments/app/main", 7, true},
		{"encoded branch slash", "https://j.example.org/job/payments/job/app/job/feature%2Fx/7/", "payments/app/feature%2Fx", 7, true},
		{"doubly encoded branch slash", "https://j.example.org/job/payments/job/app/job/feature%252Fx/7/", "payments/app/feature%2Fx", 7, true},
		{"prefix of the build number", "https://j.example.org/job/payments/job/app/job/main/42/", "payments/app/main", 4, false},
		{"another job", "https://j.example.org/job/payments/job/other/job/main/7/", "payments/app/main", 7, false},
		{"nested job with the same tail", "https://j.example.org/job/other/job/payments/job/app/job/main/7/", "payments/app/main", 7, false},
		{"context path prefix", "https://j.example.org/jenkins/job/payments/job/app/job/main/7/", "payments/app/main", 7, true},
		{"url that does not parse", "https://[::1/job/payments/job/app/job/main/7/", "payments/app/main", 7, false},
		{"empty target", "", "payments/app/main", 7, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildURLMatches(tc.target, tc.job, tc.number); got != tc.want {
				t.Fatalf("buildURLMatches(%q, %q, %d) = %v, want %v", tc.target, tc.job, tc.number, got, tc.want)
			}
		})
	}
}

// TestInterruptedStatusDescription checks the 140 rune cap.
func TestInterruptedStatusDescription(t *testing.T) {
	if got := interruptedStatusDescription("cell-lost"); got != "Varroa ended the build: cell-lost" {
		t.Fatalf("short = %q", got)
	}
	long := interruptedStatusDescription(strings.Repeat("x", 300))
	if n := len([]rune(long)); n != maxStatusDescription {
		t.Fatalf("long length = %d, want %d", n, maxStatusDescription)
	}
}

// TestEndPendingStatus_ForbiddenWriteFallsBackToNextPusher checks that a 403
// on the status write moves the build to the next pushing credential.
func TestEndPendingStatus_ForbiddenWriteFallsBackToNextPusher(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	pending := []forge.CommitStatus{{Context: "ci", State: forge.StatePending, TargetURL: statusBuildURL}}
	a := pushable()
	a.statuses = pending
	a.setStatusErr = forge.ErrForbidden
	b := pushable()
	b.statuses = pending
	twoCredentials(fx, a, b)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 1 {
		t.Fatalf("written %d, err %v; want 1, nil", n, err)
	}
	if len(a.statusWrites) != 1 || len(b.statusWrites) != 1 {
		t.Fatalf("writes a=%d b=%d, want 1 and 1", len(a.statusWrites), len(b.statusWrites))
	}
}

// TestEndPendingStatus_ForbiddenListFallsBack checks that a 403 on the status
// list moves the build to the next pushing credential.
func TestEndPendingStatus_ForbiddenListFallsBack(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.statusErr = forge.ErrForbidden
	b := pushable()
	b.statuses = []forge.CommitStatus{{Context: "ci", State: forge.StatePending, TargetURL: statusBuildURL}}
	twoCredentials(fx, a, b)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 1 {
		t.Fatalf("written %d, err %v; want 1, nil", n, err)
	}
}

// TestEndPendingStatus_AllForbiddenNamesEachCredential checks the refusal when
// every pushing credential answers 403.
func TestEndPendingStatus_AllForbiddenNamesEachCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.statusErr = forge.ErrForbidden
	b := pushable()
	b.statusErr = forge.ErrForbidden
	twoCredentials(fx, a, b)
	_, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{forgeapi.CodeForgeWriteDenied, `"a"`, `"b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v lacks %q", err, want)
		}
	}
}

// TestEndPendingStatus_ReadOnlyCredentialSkipped checks that a read-only
// credential is never listed when the pushing credential answers 403.
func TestEndPendingStatus_ReadOnlyCredentialSkipped(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.statusErr = forge.ErrForbidden
	b := pushable()
	b.info.Push = false
	twoCredentials(fx, a, b)
	if _, err := fx.ops.EndPendingStatus(context.Background(), endReq()); err == nil {
		t.Fatal("want an error")
	}
	if len(b.statusReads) != 0 {
		t.Fatalf("read-only credential listed statuses: %v", b.statusReads)
	}
}

// TestEndPendingStatus_ForbiddenWriteNeverRewritesEndedStatus checks that a
// status which the first credential ended is not written again by the next
// credential. The second credential lists the forge state after the first
// credential's write.
func TestEndPendingStatus_ForbiddenWriteNeverRewritesEndedStatus(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.statuses = []forge.CommitStatus{
		{Context: "ci", State: forge.StatePending, TargetURL: statusBuildURL},
		{Context: "lint", State: forge.StatePending, TargetURL: statusBuildURL},
	}
	a.setStatusErr = forge.ErrForbidden
	a.setStatusErrAt = 2
	b := pushable()
	b.statuses = []forge.CommitStatus{
		{Context: "ci", State: forge.StateError, TargetURL: statusBuildURL},
		{Context: "lint", State: forge.StatePending, TargetURL: statusBuildURL},
	}
	twoCredentials(fx, a, b)
	n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
	if err != nil || n != 2 {
		t.Fatalf("written %d, err %v; want 2, nil", n, err)
	}
	if len(b.statusWrites) != 1 || b.statusWrites[0].Context != "lint" {
		t.Fatalf("b writes %+v, want only the lint status", b.statusWrites)
	}
}
