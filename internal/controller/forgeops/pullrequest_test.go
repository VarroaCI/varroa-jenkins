package forgeops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const prURL = "https://git.example.org/team/app/pulls/4"

// prWriter sets one pushing credential that answers with c and returns c.
func prWriter(fx *fixture, c *fakeClient) *fakeClient {
	c.pr = forge.PullRequest{URL: prURL, Number: 4}
	oneWriter(fx, c)
	return c
}

func wantCode(t *testing.T, rep forgeapi.PullRequestReply, code string) {
	t.Helper()
	if rep.OK || rep.Code != code {
		t.Fatalf("reply %+v, want code %s", rep, code)
	}
}

func TestPullRequest_OpensBranchFilesAndPR(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := prWriter(fx, pushable())
	req := prReq()
	req.Files = append(req.Files, forgeapi.File{Path: "Makefile", Content: "all:\n"})
	rep := fx.ops.PullRequest(context.Background(), req)
	if !rep.OK || rep.URL != prURL || rep.Number != 4 {
		t.Fatalf("reply %+v", rep)
	}
	want := []string{
		"create:" + forgeapi.ProposalBranch + "@" + testSHA,
		"put:Jenkinsfile",
		"put:Makefile",
		"pr:main<-" + forgeapi.ProposalBranch,
	}
	if !reflect.DeepEqual(c.calls, want) {
		t.Fatalf("calls %v, want %v", c.calls, want)
	}
	if c.putMsgs[0] != req.CommitMessage || c.prBodies[0] != "why" {
		t.Fatalf("commit %q body %q", c.putMsgs[0], c.prBodies[0])
	}
}

func TestPullRequest_ArgumentRecheck(t *testing.T) {
	cases := map[string]func(r *forgeapi.PullRequestRequest){
		"branch dots":   func(r *forgeapi.PullRequestRequest) { r.Branch = "..x" },
		"title newline": func(r *forgeapi.PullRequestRequest) { r.Title = "a\nb" },
		"body too big":  func(r *forgeapi.PullRequestRequest) { r.Body = strings.Repeat("x", 9<<10+1) },
		"no commit":     func(r *forgeapi.PullRequestRequest) { r.CommitMessage = "" },
		"no files":      func(r *forgeapi.PullRequestRequest) { r.Files = nil },
		"nul content":   func(r *forgeapi.PullRequestRequest) { r.Files[0].Content = "a\x00" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			c := prWriter(fx, pushable())
			req := prReq()
			mutate(&req)
			wantCode(t, fx.ops.PullRequest(context.Background(), req), forgeapi.CodeInvalidRequest)
			if len(c.calls) != 0 {
				t.Fatalf("forge called: %v", c.calls)
			}
		})
	}
}

func TestPullRequest_ExistingBranchRefused(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.branches[forgeapi.ProposalBranch] = testSHA
	prWriter(fx, c)
	wantCode(t, fx.ops.PullRequest(context.Background(), prReq()), forgeapi.CodeProposalBranchExists)
	if len(c.calls) != 0 {
		t.Fatalf("writes after an existing branch: %v", c.calls)
	}
}

func TestPullRequest_BranchEqualsBaseRefused(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := prWriter(fx, pushable())
	req := prReq()
	req.Branch = "main"
	wantCode(t, fx.ops.PullRequest(context.Background(), req), forgeapi.CodeProposalBranchExists)
	if len(c.calls) != 0 {
		t.Fatalf("writes: %v", c.calls)
	}
}

func TestPullRequest_CreateBranchConflict(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.createErr = forge.ErrConflict
	prWriter(fx, c)
	wantCode(t, fx.ops.PullRequest(context.Background(), prReq()), forgeapi.CodeProposalBranchExists)
	if want := []string{"create:" + forgeapi.ProposalBranch + "@" + testSHA}; !reflect.DeepEqual(c.calls, want) {
		t.Fatalf("calls %v", c.calls)
	}
}

func TestPullRequest_CleanupAfterFailedFileWrite(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.putErr = errors.New("boom")
	c.putErrAt = 1
	prWriter(fx, c)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	wantCode(t, rep, forgeapi.CodeForgeUnreachable)
	if got := c.calls[len(c.calls)-1]; got != "delete:"+forgeapi.ProposalBranch {
		t.Fatalf("last call %q, calls %v", got, c.calls)
	}
	if c.prBodies != nil {
		t.Fatal("pull request opened after a failed write")
	}
}

func TestPullRequest_CleanupAfterFailedPullRequest(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.prErr = errors.New("boom")
	prWriter(fx, c)
	wantCode(t, fx.ops.PullRequest(context.Background(), prReq()), forgeapi.CodeForgeUnreachable)
	if got := c.calls[len(c.calls)-1]; got != "delete:"+forgeapi.ProposalBranch {
		t.Fatalf("calls %v", c.calls)
	}
}

func TestPullRequest_CleanupFailureNamesBranch(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.prErr = errors.New("boom")
	c.deleteErr = errors.New("boom")
	prWriter(fx, c)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	wantCode(t, rep, forgeapi.CodeForgeUnreachable)
	if !strings.Contains(rep.Error, `the branch "`+forgeapi.ProposalBranch+`" was not deleted`) {
		t.Fatalf("error %q", rep.Error)
	}
}

func TestPullRequest_UsesPushRule(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.info.Push = false
	prWriter(fx, c)
	wantCode(t, fx.ops.PullRequest(context.Background(), prReq()), forgeapi.CodeForgeWriteDenied)
	if len(c.calls) != 0 {
		t.Fatalf("writes without push: %v", c.calls)
	}
}

func TestPullRequest_BaseMovedRefused(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := prWriter(fx, pushable())
	req := prReq()
	req.BaseRevision = "ffffffffffffffffffffffffffffffffffffffff"
	wantCode(t, fx.ops.PullRequest(context.Background(), req), forgeapi.CodeProposalBaseMoved)
	if len(c.calls) != 0 {
		t.Fatalf("writes after the base moved: %v", c.calls)
	}
}

// TestPullRequest_ForbiddenPRFallsBackToNextPusher checks that a 403 on the
// pull request deletes the branch and opens the pull request with the next
// pushing credential.
func TestPullRequest_ForbiddenPRFallsBackToNextPusher(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.prErr = forge.ErrForbidden
	b := prWriter(fx, pushable())
	twoCredentials(fx, a, b)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	if !rep.OK || rep.URL != prURL || rep.Number != 4 {
		t.Fatalf("reply %+v", rep)
	}
	if got := a.calls[len(a.calls)-1]; got != "delete:"+forgeapi.ProposalBranch {
		t.Fatalf("a calls %v, want the branch deleted last", a.calls)
	}
	want := []string{
		"create:" + forgeapi.ProposalBranch + "@" + testSHA,
		"put:Jenkinsfile",
		"pr:main<-" + forgeapi.ProposalBranch,
	}
	if !reflect.DeepEqual(b.calls, want) {
		t.Fatalf("b calls %v, want %v", b.calls, want)
	}
	if !strings.Contains(fx.logBuf.String(), "trying the next credential") {
		t.Fatalf("log %q lacks the fallback line", fx.logBuf.String())
	}
}

// TestPullRequest_ForbiddenCreateBranchFallsBack checks that a 403 on the
// branch create moves on without a delete, since no branch exists yet.
func TestPullRequest_ForbiddenCreateBranchFallsBack(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.createErr = forge.ErrForbidden
	b := prWriter(fx, pushable())
	twoCredentials(fx, a, b)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	if !rep.OK {
		t.Fatalf("reply %+v", rep)
	}
	for _, call := range a.calls {
		if strings.HasPrefix(call, "delete:") {
			t.Fatalf("a deleted a branch it never created: %v", a.calls)
		}
	}
	if len(b.calls) == 0 {
		t.Fatal("b was not tried")
	}
}

// TestPullRequest_AllForbiddenNamesEachCredential checks the refusal when
// every pushing credential answers 403. It names each credential and never
// a token.
func TestPullRequest_AllForbiddenNamesEachCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.prErr = forge.ErrForbidden
	b := pushable()
	b.prErr = forge.ErrForbidden
	twoCredentials(fx, a, b)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	wantCode(t, rep, forgeapi.CodeForgeWriteDenied)
	for _, want := range []string{`"a"`, `"b"`, "check the token scope"} {
		if !strings.Contains(rep.Error, want) {
			t.Fatalf("error %q lacks %q", rep.Error, want)
		}
	}
	if strings.Contains(rep.Error, "tok-") {
		t.Fatalf("error %q names a token", rep.Error)
	}
	for name, c := range map[string]*fakeClient{"a": a, "b": b} {
		if got := c.calls[len(c.calls)-1]; got != "delete:"+forgeapi.ProposalBranch {
			t.Fatalf("%s calls %v, want the branch deleted last", name, c.calls)
		}
	}
}

// TestPullRequest_ForbiddenCleanupFailureStops checks that a failed delete
// after a 403 stops the attempt. The next credential is never tried.
func TestPullRequest_ForbiddenCleanupFailureStops(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.prErr = forge.ErrForbidden
	a.deleteErr = errors.New("x")
	b := prWriter(fx, pushable())
	twoCredentials(fx, a, b)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	wantCode(t, rep, forgeapi.CodeForgeWriteDenied)
	if !strings.Contains(rep.Error, "was not deleted") {
		t.Fatalf("error %q", rep.Error)
	}
	if len(b.calls) != 0 {
		t.Fatalf("b was tried after a failed delete: %v", b.calls)
	}
}

// TestPullRequest_NoFallbackOnOtherError checks that a non-403 error stops
// the attempt, and the next credential is never tried.
func TestPullRequest_NoFallbackOnOtherError(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.prErr = errors.New("boom")
	b := prWriter(fx, pushable())
	twoCredentials(fx, a, b)
	wantCode(t, fx.ops.PullRequest(context.Background(), prReq()), forgeapi.CodeForgeUnreachable)
	if len(b.calls) != 0 {
		t.Fatalf("b was tried after a non-403 error: %v", b.calls)
	}
}

// TestPullRequest_ReadOnlyCredentialNeverTried checks that a read-only
// credential is not a fallback for a write.
func TestPullRequest_ReadOnlyCredentialNeverTried(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.prErr = forge.ErrForbidden
	b := prWriter(fx, pushable())
	b.info.Push = false
	twoCredentials(fx, a, b)
	rep := fx.ops.PullRequest(context.Background(), prReq())
	wantCode(t, rep, forgeapi.CodeForgeWriteDenied)
	if len(b.calls) != 0 {
		t.Fatalf("read-only credential wrote: %v", b.calls)
	}
	if strings.Contains(rep.Error, `"b"`) {
		t.Fatalf("error %q names the read-only credential", rep.Error)
	}
}
