package forgeops

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// maxStatusDescription bounds the description of a commit status, in runes.
const maxStatusDescription = 140

// maxUnescapePasses bounds how often splitJobURL decodes a path segment.
// A branch name can hold an encoded slash, and a URL can encode it again.
const maxUnescapePasses = 3

// EndPendingStatus ends each pending commit status of one build. A Reason
// request sets error. A Result request sets the state that the Jenkins result
// maps to. A context is changed only when its latest state is pending. A
// Reason request changes a context whose target URL is the build URL of this
// job and build number under req.JenkinsURL. A Result request also changes a
// context whose target URL is the URL of the job itself under req.JenkinsURL.
// The plugin writes the queued status with that URL, and a dead branch job
// queues no other build. A Reason request never does this, because an
// automatic retry's queued status has that URL. A context with an end state,
// or a target URL of another Jenkins, is never changed. A request with no
// JenkinsURL is refused, because the operator then cannot tell this
// Jenkins's statuses from another Jenkins's. It tries each pushing credential
// in id order. A forge 403 moves to the next credential. A rate limit stops
// the attempt. It returns the number of statuses that it wrote.
func (o *Ops) EndPendingStatus(ctx context.Context, req forgeapi.CommitStatusRequest) (int, error) {
	if req.Tenant == "" || req.Repo == "" || req.SHA == "" || req.JobPath == "" || req.JenkinsURL == "" {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "tenant, repo, sha, jobPath and jenkinsURL are required")
	}
	if (req.Reason == "") == (req.Result == "") {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "exactly one of reason and result is required")
	}
	if req.BuildNumber <= 0 {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "build number must be positive")
	}
	return o.writeStatuses(ctx, req.Tenant, req.Repo, func(w writer, fr forge.Repo) (int, error) {
		return o.endStatusesWith(ctx, w, fr, req)
	})
}

// writeStatuses runs write with each pushing credential of the tenant for
// the repository, in id order. A forge 403 moves to the next credential. A
// rate limit or any other error stops the attempt. It returns the number of
// statuses that the writes wrote.
func (o *Ops) writeStatuses(ctx context.Context, tenant, repoURL string, write func(w writer, fr forge.Repo) (int, error)) (int, error) {
	repo, err := forgeurl.NormalizeRepoURL(repoURL)
	if err != nil {
		return 0, replyErr(forgeapi.CodeRepoURLInvalid, "%v", err)
	}
	creds, err := o.chooseCredential(ctx, tenant, repo.Host)
	if err != nil {
		return 0, err
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	cands, err := o.candidates(ctx, repo.Host, fr, creds, true)
	if err != nil {
		return 0, err
	}
	written := 0
	var forbidden []string
	for _, w := range cands {
		n, err := write(w, fr)
		written += n
		if errors.Is(err, forge.ErrForbidden) {
			o.logger.Info("forge commit status forbidden; trying the next credential", "credential", w.CredentialID, "host", repo.Host)
			forbidden = append(forbidden, w.CredentialID)
			continue
		}
		if err != nil {
			return written, forgeFailure(err, w.CredentialID)
		}
		return written, nil
	}
	return written, forbiddenRefusal(forbidden)
}

// endStatusesWith ends the pending statuses of the build with credential w. It
// returns the count written and the first forge error, unchanged. A later
// credential lists the statuses again. A status that an earlier credential
// ended is not pending, so it is not written twice.
func (o *Ops) endStatusesWith(ctx context.Context, w writer, fr forge.Repo, req forgeapi.CommitStatusRequest) (int, error) {
	statuses, err := w.Client.CommitStatuses(ctx, fr, req.SHA)
	if err != nil {
		return 0, err
	}
	state, description := endState(w.Kind, req)
	written := 0
	for _, s := range statuses {
		if s.State != forge.StatePending || !statusOfBuild(s.TargetURL, req) {
			continue
		}
		next := forge.CommitStatus{
			Context:     s.Context,
			State:       state,
			TargetURL:   s.TargetURL,
			Description: description,
		}
		if err := w.Client.SetCommitStatus(ctx, fr, req.SHA, next); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// statusOfBuild reports whether a pending status belongs to the request. A
// Result request also matches the job URL of its branch job.
func statusOfBuild(targetURL string, req forgeapi.CommitStatusRequest) bool {
	if buildURLUnder(targetURL, req.JenkinsURL, req.JobPath, req.BuildNumber) {
		return true
	}
	return req.Result != "" && jobURLUnder(targetURL, req.JenkinsURL, req.JobPath)
}

// splitJobURL splits targetURL after the job chain of jobPath. root is the
// Jenkins location URL. The scheme and the host of targetURL must equal
// those of root, and its path must start with the path of root. One
// job/<name> pair for each segment of jobPath must follow that path. Each
// segment is compared after unescapeFully, so an encoded branch slash and a
// doubly encoded URL both match. It answers the parsed URL, its escaped path
// segments, and the index of the first segment after the job chain. Any other
// URL answers false, so a status of another Jenkins with the same job path is
// never matched. Every commit-status write matches a target URL through this
// function.
func splitJobURL(targetURL, root, jobPath string) (*url.URL, []string, int, bool) {
	u, err := url.Parse(targetURL)
	if err != nil || u.Opaque != "" || u.User != nil {
		return nil, nil, 0, false
	}
	r, err := url.Parse(root)
	if err != nil || r.Host == "" || r.Opaque != "" {
		return nil, nil, 0, false
	}
	if !strings.EqualFold(u.Scheme, r.Scheme) || !strings.EqualFold(u.Host, r.Host) {
		return nil, nil, 0, false
	}
	rootSegs := strings.Split(strings.TrimSuffix(r.EscapedPath(), "/"), "/")
	segs := strings.Split(u.EscapedPath(), "/")
	if len(segs) < len(rootSegs) {
		return nil, nil, 0, false
	}
	for k, s := range rootSegs {
		if unescapeFully(segs[k]) != unescapeFully(s) {
			return nil, nil, 0, false
		}
	}
	i := len(rootSegs)
	for _, name := range strings.Split(jobPath, "/") {
		if i+1 >= len(segs) || segs[i] != "job" || unescapeFully(segs[i+1]) != unescapeFully(name) {
			return nil, nil, 0, false
		}
		i += 2
	}
	return u, segs, i, true
}

// splitBuildURL answers what splitJobURL answers, with the index of the
// build-number segment, when targetURL is the build URL of build number of
// jobPath under root. The number must follow the job chain, and linkTail must
// accept the rest. Build 4 does not match build 42, and a page under the
// build does not match.
func splitBuildURL(targetURL, root, jobPath string, number int64) (*url.URL, []string, int, bool) {
	u, segs, i, ok := splitJobURL(targetURL, root, jobPath)
	if !ok || i >= len(segs) || unescapeFully(segs[i]) != strconv.FormatInt(number, 10) || !linkTail(segs[i+1:]) {
		return nil, nil, 0, false
	}
	return u, segs, i, true
}

// buildURLUnder reports whether targetURL is the build URL of build number
// of jobPath under the Jenkins location URL root.
func buildURLUnder(targetURL, root, jobPath string, number int64) bool {
	_, _, _, ok := splitBuildURL(targetURL, root, jobPath, number)
	return ok
}

// jobURLUnder reports whether targetURL is the URL of the job jobPath itself
// under the Jenkins location URL root, with no build number. linkTail must
// accept the segments after the job chain.
func jobURLUnder(targetURL, root, jobPath string) bool {
	_, segs, i, ok := splitJobURL(targetURL, root, jobPath)
	return ok && linkTail(segs[i:])
}

// linkTail reports whether rest, the path segments after a build number or a
// job chain, is one of the tails that the display-url-api plugin gives a
// branch-source plugin: nothing, a slash, or display/redirect with an
// optional slash.
func linkTail(rest []string) bool {
	switch len(rest) {
	case 0:
		return true
	case 1:
		return rest[0] == ""
	case 2:
		return rest[0] == "display" && rest[1] == "redirect"
	case 3:
		return rest[0] == "display" && rest[1] == "redirect" && rest[2] == ""
	}
	return false
}

// endState returns the state and the description that end a pending status
// on a forge of kind.
func endState(kind forge.Kind, req forgeapi.CommitStatusRequest) (string, string) {
	if req.Result == "" {
		return forge.StateError, interruptedStatusDescription(req.Reason)
	}
	return resultState(kind, req.Result), completedStatusDescription(req.Result)
}

// resultState maps a Jenkins result to the commit status state that the
// branch-source plugin of kind writes for it. The github-branch-source plugin
// writes error for FAILURE, and the gitea plugin writes failure. Both write
// failure for UNSTABLE and error for ABORTED. For NOT_BUILT, the gitea plugin
// reads the forge version and writes skipped or warning. This table reads no
// version, so it writes error, as it does for every other result.
func resultState(kind forge.Kind, result string) string {
	switch result {
	case "SUCCESS":
		return forge.StateSuccess
	case "UNSTABLE":
		return forge.StateFailure
	case "FAILURE":
		if kind == forge.KindGitHub {
			return forge.StateError
		}
		return forge.StateFailure
	default:
		return forge.StateError
	}
}

// unescapeFully decodes s until it stops changing, at most maxUnescapePasses
// times. A value that does not decode is returned as it was.
func unescapeFully(s string) string {
	for i := 0; i < maxUnescapePasses; i++ {
		dec, err := url.PathUnescape(s)
		if err != nil || dec == s {
			return s
		}
		s = dec
	}
	return s
}

// interruptedStatusDescription returns the description that replaces a
// pending status after an interrupted run.
func interruptedStatusDescription(reason string) string {
	return capStatusDescription("Varroa ended the build: " + reason)
}

// completedStatusDescription returns the description that replaces a pending
// status after a completed run of a dead branch job.
func completedStatusDescription(result string) string {
	return capStatusDescription("Varroa reported the build result: " + result)
}

// capStatusDescription holds d to at most maxStatusDescription runes.
func capStatusDescription(d string) string {
	if utf8.RuneCountInString(d) <= maxStatusDescription {
		return d
	}
	r := []rune(d)
	return string(r[:maxStatusDescription])
}
