package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// codeOperatorTimeout reports that the operator did not answer a forge request
// before the relay stopped waiting. After a reply timeout, the forge write may
// have landed.
const codeOperatorTimeout = "operator_timeout"

// isOperatorTimeout reports whether the relay stopped waiting for the operator.
// The bus returns nats.ErrTimeout or nats.ErrNoResponders when the reply does not
// come, and the caller's context returns context.DeadlineExceeded.
func isOperatorTimeout(err error) bool {
	return bus.IsNoReply(err) || errors.Is(err, context.DeadlineExceeded)
}

// OpenPullRequestArgs are the model's arguments of open_pull_request.
type OpenPullRequestArgs struct {
	Team   string
	Repo   string
	Branch string
	Title  string
	Body   string
	Files  []forgeapi.File
}

// OpenPullRequestResult names the pull request and the cluster that opened it.
type OpenPullRequestResult struct {
	URL     string
	Number  int
	Cluster string
}

// OpenPullRequest serves the open_pull_request tool. Every refusal is a
// ServiceError whose message starts with the tool error code, then a colon.
//
// Inside an assistant session the repo must be the session's own repo. The
// body and the commit message then name the session, so the pull request
// traces to it.
func (s *Server) OpenPullRequest(ctx context.Context, claims *auth.Claims, a OpenPullRequestArgs) (OpenPullRequestResult, *ServiceError) {
	if s.deps.ForgeRelay == nil {
		return OpenPullRequestResult{}, toolErr(http.StatusServiceUnavailable, forgeapi.CodeForgeUnreachable, "the forge relay is not configured")
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, a.Team)
	ref, ok := teamPipelinesTenant(refs, a.Team)
	if serr != nil || !ok || !s.canReadPipeline(claims, ref, "") {
		return OpenPullRequestResult{}, toolErr(http.StatusNotFound, "not_found", "team not found")
	}
	if !s.canWritePipeline(claims, ref, "") {
		return OpenPullRequestResult{}, toolErr(http.StatusForbidden, "forbidden", "the tenant write check refused the call")
	}

	sess, inSession, err := s.teamAgentSession(ctx)
	if err != nil {
		return OpenPullRequestResult{}, toolErr(http.StatusServiceUnavailable, "internal", "session snapshot is unavailable")
	}
	baseRevision := ""
	if inSession {
		baseRevision, _ = sess.Context["baseRevision"].(string)
		sessRepo, _ := sess.Context["repo"].(string)
		if canonicalRepo(a.Repo) != canonicalRepo(sessRepo) {
			return OpenPullRequestResult{}, toolErr(http.StatusBadRequest, "repo_not_in_session", "repo %q is not the repository of this session", a.Repo)
		}
	}

	u, err := forgeurl.NormalizeRepoURL(a.Repo)
	if err != nil {
		return OpenPullRequestResult{}, toolErr(http.StatusBadRequest, forgeapi.CodeRepoURLInvalid, "%v", err)
	}
	if err := firstRuleError(
		forgeurl.ValidateBranch(a.Branch),
		forgeurl.ValidateTitle(a.Title),
		forgeurl.ValidateBody(a.Body),
		forgeurl.ValidateFiles(ruleFiles(a.Files)),
	); err != nil {
		return OpenPullRequestResult{}, toolErr(http.StatusBadRequest, "invalid_argument", "%v", err)
	}

	body := a.Body
	if body != "" {
		body += "\n\n"
	}
	body += "Opened through Varroa by " + ActorFrom(claims) + "."
	msg := a.Title
	if inSession {
		body += " Assistant session " + sess.ID.String() + "."
		msg += "\n\nVarroa-Session: " + sess.ID.String()
	}

	cluster := ref.Cluster
	if cluster == "" {
		cluster = s.deps.CoreCluster
	}
	reply, err := s.deps.ForgeRelay.PullRequest(ctx, cluster, forgeapi.PullRequestRequest{
		Team: a.Team, Repo: u.String(), Branch: a.Branch, Title: a.Title, Body: body,
		Files: a.Files, CommitMessage: msg, BaseRevision: baseRevision,
	})
	if err != nil && isOperatorTimeout(err) {
		return OpenPullRequestResult{}, toolErr(http.StatusGatewayTimeout, codeOperatorTimeout, "the operator did not answer in time. Call again later.")
	}
	if err != nil {
		return OpenPullRequestResult{}, toolErr(http.StatusBadGateway, forgeapi.CodeForgeUnreachable, "%v", err)
	}
	if !reply.OK {
		return OpenPullRequestResult{}, toolErr(forgeCodeStatus(reply.Code), reply.Code, "%s", reply.Error)
	}
	return OpenPullRequestResult{URL: reply.URL, Number: reply.Number, Cluster: cluster}, nil
}

// toolErr builds a ServiceError whose message starts with the tool error code.
func toolErr(status int, code, format string, args ...any) *ServiceError {
	return &ServiceError{Status: status, Message: code + ": " + fmt.Sprintf(format, args...)}
}

// canonicalRepo returns the normalized form of a repository URL, or the input
// when it does not normalize. Two spellings of one repository compare equal.
func canonicalRepo(raw string) string {
	if u, err := forgeurl.NormalizeRepoURL(raw); err == nil {
		return u.String()
	}
	return raw
}

// ruleFiles converts the wire files to the rule type.
func ruleFiles(files []forgeapi.File) []forgeurl.File {
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
