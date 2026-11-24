package forgeops

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// githubBranchSourceContexts are the status contexts that the
// github-branch-source plugin writes by default: one for a branch, one for
// the merge build and one for the head build of a pull request.
var githubBranchSourceContexts = []string{
	"continuous-integration/jenkins/branch",
	"continuous-integration/jenkins/pr-merge",
	"continuous-integration/jenkins/pr-head",
}

// RestoreNewestStatus puts the result of the newest build of a job back on
// a commit. It reads the latest status of each context. It writes only a
// context that the branch-source plugin of the forge writes by default, and
// whose latest status is a build URL of a superseded build under
// req.JenkinsURL, whatever that state is. The new status keeps the context,
// sets the state that req.Result maps to, and points the target URL at
// req.BuildNumber. Every other context is never written. The credential
// rules of EndPendingStatus apply. It returns the number of statuses that it
// wrote.
func (o *Ops) RestoreNewestStatus(ctx context.Context, req forgeapi.NewestStatusRequest) (int, error) {
	if req.Tenant == "" || req.Repo == "" || req.SHA == "" || req.JobPath == "" || req.Result == "" || req.JenkinsURL == "" {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "tenant, repo, sha, jobPath, result and jenkinsURL are required")
	}
	if req.BuildNumber <= 0 {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "build number must be positive")
	}
	if len(req.SupersededBuilds) == 0 {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "superseded builds are required")
	}
	return o.writeStatuses(ctx, req.Tenant, req.Repo, func(w writer, fr forge.Repo) (int, error) {
		return o.restoreStatusesWith(ctx, w, fr, req)
	})
}

// restoreStatusesWith writes the newest build's state over each context of
// the commit that restorableContext accepts. It returns the count written and
// the first forge error, unchanged. A later credential reads the statuses
// again, so a context that an earlier credential wrote names the newest build
// and is not written twice.
func (o *Ops) restoreStatusesWith(ctx context.Context, w writer, fr forge.Repo, req forgeapi.NewestStatusRequest) (int, error) {
	statuses, err := w.Client.CommitStatuses(ctx, fr, req.SHA)
	if err != nil {
		return 0, err
	}
	state := resultState(w.Kind, req.Result)
	description := newestStatusDescription(req.BuildNumber, req.Result)
	written := 0
	for _, s := range statuses {
		target, ok := restorableContext(w.Kind, s, req)
		if !ok {
			continue
		}
		next := forge.CommitStatus{
			Context:     s.Context,
			State:       state,
			TargetURL:   target,
			Description: description,
		}
		if err := w.Client.SetCommitStatus(ctx, fr, req.SHA, next); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// restorableContext returns the target URL of the newest build when s is a
// default branch-source context of kind for the job, and its target URL is a
// build URL of a superseded build under the Jenkins location URL. The answer
// is false for every other status.
func restorableContext(kind forge.Kind, s forge.CommitStatus, req forgeapi.NewestStatusRequest) (string, bool) {
	if !branchSourceContext(kind, s.Context, req.JobPath) {
		return "", false
	}
	for _, old := range req.SupersededBuilds {
		if old == req.BuildNumber {
			continue
		}
		if target, ok := replaceBuildNumber(s.TargetURL, req.JenkinsURL, req.JobPath, old, req.BuildNumber); ok {
			return target, true
		}
	}
	return "", false
}

// branchSourceContext reports whether name is a status context that the
// branch-source plugin of kind writes by default for a build of jobPath. The
// github-branch-source plugin writes one of githubBranchSourceContexts. The
// gitea plugin writes the full name of the job's parent, then /pipeline/,
// then head, tag, release, or pr- and the target branch. A context that a
// pipeline step or another plugin writes is never one of them.
func branchSourceContext(kind forge.Kind, name, jobPath string) bool {
	switch kind {
	case forge.KindGitHub:
		for _, c := range githubBranchSourceContexts {
			if name == c {
				return true
			}
		}
	case forge.KindGitea:
		i := strings.LastIndex(jobPath, "/")
		if i <= 0 {
			return false
		}
		rest, ok := strings.CutPrefix(name, jobPath[:i]+"/pipeline/")
		if !ok {
			return false
		}
		switch {
		case rest == "head", rest == "tag", rest == "release":
			return true
		case strings.HasPrefix(rest, "pr-") && len(rest) > len("pr-"):
			return true
		}
	}
	return false
}

// replaceBuildNumber returns targetURL with build from of jobPath set to
// build to. splitBuildURL must accept targetURL as the build URL of build
// from under the Jenkins location URL root. The query stays as it was. Any
// other URL answers false, so a status of another Jenkins with the same job
// path and build number is never written.
func replaceBuildNumber(targetURL, root, jobPath string, from, to int64) (string, bool) {
	u, segs, i, ok := splitBuildURL(targetURL, root, jobPath, from)
	if !ok {
		return "", false
	}
	segs[i] = strconv.FormatInt(to, 10)
	escaped := strings.Join(segs, "/")
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false
	}
	u.Path = path
	u.RawPath = escaped
	return u.String(), true
}

// newestStatusDescription returns the description of a status that the
// newest build of a job gets back.
func newestStatusDescription(build int64, result string) string {
	return capStatusDescription("Varroa kept the result of the newest build #" +
		strconv.FormatInt(build, 10) + ": " + result)
}
