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
