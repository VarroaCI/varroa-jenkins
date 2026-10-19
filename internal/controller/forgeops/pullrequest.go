package forgeops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// cleanupTimeout bounds the branch delete after a failed write. It runs on
// a fresh context, so a spent request deadline does not skip the delete.
const cleanupTimeout = 15 * time.Second

// PullRequest opens one pull request: a new branch from the base head, one
// commit per file, and the pull request. A failure after the branch exists
// deletes the branch once.
func (o *Ops) PullRequest(ctx context.Context, req forgeapi.PullRequestRequest) forgeapi.PullRequestReply {
	fail := func(err error) forgeapi.PullRequestReply { return forgeapi.PullRequestReply{Status: asStatus(err)} }
	if req.Team == "" {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team is required"))
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return fail(replyErr(forgeapi.CodeRepoURLInvalid, "%v", err))
	}
	if err := firstRuleError(
		forgeurl.ValidateBranch(req.Branch),
		forgeurl.ValidateTitle(req.Title),
		forgeurl.ValidateRelayBody(req.Body),
		forgeurl.ValidateCommitMessage(req.CommitMessage),
		forgeurl.ValidateFiles(toRuleFiles(req.Files)),
	); err != nil {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "%v", err))
	}
	creds, err := o.chooseCredential(ctx, req.Team, repo.Host)
	if err != nil {
		return fail(err)
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	w, err := o.pickWriter(ctx, repo.Host, fr, creds)
	if err != nil {
		return fail(err)
	}

	base := w.Info.DefaultBranch
	baseSHA, ok, err := w.Client.BranchHead(ctx, fr, base)
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	if !ok {
		return fail(replyErr(forgeapi.CodeNotFound, "base branch %q has no commit", base))
	}
	if req.BaseRevision != "" && baseSHA != req.BaseRevision {
		return fail(replyErr(forgeapi.CodeProposalBaseMoved, "base branch %q moved after the check", base))
	}
	if req.Branch == base {
		return fail(replyErr(forgeapi.CodeProposalBranchExists, "the branch %q is the base branch", req.Branch))
	}
	_, exists, err := w.Client.BranchHead(ctx, fr, req.Branch)
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	if exists {
		return fail(replyErr(forgeapi.CodeProposalBranchExists, "branch %q already exists. Delete the branch or merge its pull request.", req.Branch))
	}
	// No branch exists yet, so a failure here needs no cleanup.
	if err := w.Client.CreateBranch(ctx, fr, req.Branch, baseSHA); err != nil {
		if errors.Is(err, forge.ErrConflict) {
			return fail(replyErr(forgeapi.CodeProposalBranchExists, "branch %q already exists", req.Branch))
		}
		return fail(forgeFailure(err, w.CredentialID))
	}
	for _, f := range req.Files {
		if err := w.Client.PutFile(ctx, fr, req.Branch, f.Path, []byte(f.Content), req.CommitMessage); err != nil {
			if errors.Is(err, forge.ErrConflict) {
				return o.cleanup(ctx, w, fr, req.Branch, replyErr(forgeapi.CodeInvalidRequest, "path %q conflicts with a directory", f.Path))
			}
			return o.cleanup(ctx, w, fr, req.Branch, forgeFailure(err, w.CredentialID))
		}
	}
	pr, err := w.Client.OpenPullRequest(ctx, fr, base, req.Branch, req.Title, req.Body)
	if err != nil {
		if errors.Is(err, forge.ErrConflict) {
			// A pull request may already use this head. Deleting the branch would
			// close it, so the branch stays.
			return fail(replyErr(forgeapi.CodeProposalBranchExists, "a pull request for branch %q already exists", req.Branch))
		}
		return o.cleanup(ctx, w, fr, req.Branch, forgeFailure(err, w.CredentialID))
	}
	return forgeapi.PullRequestReply{Status: forgeapi.Status{OK: true}, URL: pr.URL, Number: pr.Number}
}

// cleanup deletes the branch once and returns the reply for the first error.
func (o *Ops) cleanup(ctx context.Context, w writer, fr forge.Repo, branch string, first error) forgeapi.PullRequestReply {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	st := asStatus(first)
	if err := w.Client.DeleteBranch(dctx, fr, branch); err != nil {
		o.logger.Warn("forge branch cleanup failed", "branch", branch, "credential", w.CredentialID, "error", err)
		st.Error += fmt.Sprintf("; the branch %q was not deleted, delete it by hand", branch)
	}
	return forgeapi.PullRequestReply{Status: st}
}

// toRuleFiles converts the wire files to the rule type.
func toRuleFiles(files []forgeapi.File) []forgeurl.File {
	out := make([]forgeurl.File, 0, len(files))
	for _, f := range files {
		out = append(out, forgeurl.File{Path: f.Path, Content: f.Content})
	}
	return out
}

// firstRuleError returns the first non-nil error of errs.
func firstRuleError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
