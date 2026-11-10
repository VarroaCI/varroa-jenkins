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

// maxUnescapePasses bounds how often buildURLMatches decodes a target URL.
// A branch name can hold an encoded slash, and a URL can encode it again.
const maxUnescapePasses = 3

// EndPendingStatus ends each pending commit status of one build. A Reason
// request sets error. A Result request sets the state that the Jenkins result
// maps to. A context is changed only when its latest state is pending. A
// Reason request changes a context whose target URL names this job and build
// number. A Result request also changes a context whose target URL names the
// job itself. The plugin writes the queued status with that URL, and a dead
// branch job queues no other build. A Reason request never does this, because
// an automatic retry's queued status has that URL. A context with an end state
// is never changed. It tries each pushing credential in id order. A forge 403
// moves to the next credential. A rate limit stops the attempt. It returns the
// number of statuses that it wrote.
func (o *Ops) EndPendingStatus(ctx context.Context, req forgeapi.CommitStatusRequest) (int, error) {
	if req.Tenant == "" || req.Repo == "" || req.SHA == "" || req.JobPath == "" {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "tenant, repo, sha and jobPath are required")
	}
	if (req.Reason == "") == (req.Result == "") {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "exactly one of reason and result is required")
	}
	if req.BuildNumber <= 0 {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "build number must be positive")
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return 0, replyErr(forgeapi.CodeRepoURLInvalid, "%v", err)
	}
	creds, err := o.chooseCredential(ctx, req.Tenant, repo.Host)
	if err != nil {
		return 0, err
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	cands, err := o.candidates(ctx, repo.Host, fr, creds, true)
	if err != nil {
		return 0, err
	}
	state, description := endState(req)
	written := 0
	var forbidden []string
	for _, w := range cands {
		n, err := o.endStatusesWith(ctx, w, fr, req, state, description)
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
func (o *Ops) endStatusesWith(ctx context.Context, w writer, fr forge.Repo, req forgeapi.CommitStatusRequest, state, description string) (int, error) {
	statuses, err := w.Client.CommitStatuses(ctx, fr, req.SHA)
	if err != nil {
		return 0, err
	}
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

// buildURLMatches reports whether targetURL names the build jobPath number.
// The URL is decoded until it stops changing, so an encoded branch slash
// and a doubly encoded URL both match. The number must end the path, so
// build 4 does not match build 42.
func buildURLMatches(targetURL, jobPath string, number int64) bool {
	u, err := url.Parse(targetURL)
	if err != nil {
		return false
	}
	num := strconv.FormatInt(number, 10)
	segs := strings.Split(u.EscapedPath(), "/")
	for i := range segs {
		if unescapeFully(segs[i]) == num && jobChainEndsAt(segs, i, jobPath) {
			return true
		}
	}
	return false
}

// statusOfBuild reports whether a pending status belongs to the request. A
// Result request also matches the job URL of its branch job.
func statusOfBuild(targetURL string, req forgeapi.CommitStatusRequest) bool {
	if buildURLMatches(targetURL, req.JobPath, req.BuildNumber) {
		return true
	}
	return req.Result != "" && jobURLMatches(targetURL, req.JobPath)
}

// jobURLMatches reports whether targetURL names the job jobPath itself, with
// no build number. A trailing display/redirect suffix is allowed.
func jobURLMatches(targetURL, jobPath string) bool {
	u, err := url.Parse(targetURL)
	if err != nil {
		return false
	}
	segs := strings.Split(strings.TrimSuffix(u.EscapedPath(), "/"), "/")
	if n := len(segs); n >= 2 && segs[n-2] == "display" && segs[n-1] == "redirect" {
		segs = segs[:n-2]
	}
	return jobChainEndsAt(segs, len(segs), jobPath)
}

// jobChainEndsAt reports whether the job/<name> pairs that end before index
// end equal the whole job path. The walk stops at the first segment that is
// not a job pair, so a headless prefix does not stop the match.
func jobChainEndsAt(segs []string, end int, jobPath string) bool {
	want := strings.Split(jobPath, "/")
	for k := range want {
		want[k] = unescapeFully(want[k])
	}
	var chain []string
	for j := end - 1; j >= 1 && unescapeFully(segs[j-1]) == "job"; j -= 2 {
		chain = append(chain, unescapeFully(segs[j]))
	}
	return len(chain) == len(want) && equalReversed(chain, want)
}

// endState returns the state and the description that end a pending status.
func endState(req forgeapi.CommitStatusRequest) (string, string) {
	if req.Result == "" {
		return forge.StateError, interruptedStatusDescription(req.Reason)
	}
	return resultState(req.Result), completedStatusDescription(req.Result)
}

// resultState maps a Jenkins result to a commit status state. A result with
// no forge equivalent, such as NOT_BUILT, maps to error.
func resultState(result string) string {
	switch result {
	case "SUCCESS":
		return forge.StateSuccess
	case "UNSTABLE", "FAILURE":
		return forge.StateFailure
	default:
		return forge.StateError
	}
}

// equalReversed reports whether chain, read from its end, equals want.
func equalReversed(chain, want []string) bool {
	for k := range want {
		if chain[len(chain)-1-k] != want[k] {
			return false
		}
	}
	return true
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
