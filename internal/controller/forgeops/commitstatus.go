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

// EndPendingStatus sets each pending commit status of one build to error.
// Only the branch-source plugins write a status, and they never end it when
// the recorder ends the build. A context is changed only when its latest
// state is pending and its target URL names this job and build number.
// A context with an end state is never changed. It tries each pushing
// credential in id order. A forge 403 moves to the next credential. A rate
// limit stops the attempt. It
// returns the number of statuses that it wrote.
func (o *Ops) EndPendingStatus(ctx context.Context, req forgeapi.CommitStatusRequest) (int, error) {
	if req.Team == "" || req.Repo == "" || req.SHA == "" || req.JobPath == "" || req.Reason == "" {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "team, repo, sha, jobPath and reason are required")
	}
	if req.BuildNumber <= 0 {
		return 0, replyErr(forgeapi.CodeInvalidRequest, "build number must be positive")
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return 0, replyErr(forgeapi.CodeRepoURLInvalid, "%v", err)
	}
	creds, err := o.chooseCredential(ctx, req.Team, repo.Host)
	if err != nil {
		return 0, err
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	cands, err := o.candidates(ctx, repo.Host, fr, creds, true)
	if err != nil {
		return 0, err
	}
	description := interruptedStatusDescription(req.Reason)
	written := 0
	var forbidden []string
	for _, w := range cands {
		n, err := o.endStatusesWith(ctx, w, fr, req, description)
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
func (o *Ops) endStatusesWith(ctx context.Context, w writer, fr forge.Repo, req forgeapi.CommitStatusRequest, description string) (int, error) {
	statuses, err := w.Client.CommitStatuses(ctx, fr, req.SHA)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, s := range statuses {
		if s.State != forge.StatePending || !buildURLMatches(s.TargetURL, req.JobPath, req.BuildNumber) {
			continue
		}
		next := forge.CommitStatus{
			Context:     s.Context,
			State:       forge.StateError,
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
	want := make([]string, 0, 4)
	for _, name := range strings.Split(jobPath, "/") {
		want = append(want, unescapeFully(name))
	}
	num := strconv.FormatInt(number, 10)
	segs := strings.Split(u.EscapedPath(), "/")
	for i := range segs {
		if unescapeFully(segs[i]) != num {
			continue
		}
		// Walk back over the job/<name> pairs that end at this build number.
		// The chain must equal the whole job path, so a nested job of the
		// same tail does not match.
		var chain []string
		for j := i - 1; j >= 1 && unescapeFully(segs[j-1]) == "job"; j -= 2 {
			chain = append(chain, unescapeFully(segs[j]))
		}
		if len(chain) == len(want) && equalReversed(chain, want) {
			return true
		}
	}
	return false
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
// pending status. It holds at most maxStatusDescription runes.
func interruptedStatusDescription(reason string) string {
	d := "Varroa ended the build: " + reason
	if utf8.RuneCountInString(d) <= maxStatusDescription {
		return d
	}
	r := []rune(d)
	return string(r[:maxStatusDescription])
}
