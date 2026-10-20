package forgeops

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// maxIssueTitle bounds the title of an issue, in runes.
const maxIssueTitle = 256

// issueCandidates returns every credential that reads the repository, in
// order: the credentials that may push first, then the read-only ones. Each
// group keeps the credential id order. The forge decides the issue write.
func (o *Ops) issueCandidates(ctx context.Context, host string, repo forge.Repo, creds []Credential) ([]writer, error) {
	kind, err := o.detect(ctx, o.http, host)
	if errors.Is(err, forge.ErrUnsupported) {
		return nil, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
	}
	if err != nil {
		o.logger.Warn("forge detect failed", "host", host, "error", err)
		return nil, replyErr(forgeapi.CodeForgeUnreachable, "forge %q is unreachable", host)
	}
	var pushers, readers []writer
	var otherErr error
	otherID := ""
	authed := false
	for _, c := range creds {
		cl := o.newClient(kind, host, c.Token, o.http)
		if cl == nil {
			return nil, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
		}
		info, err := cl.Repository(ctx, repo)
		w := writer{Kind: kind, Client: cl, CredentialID: c.ID, Info: info}
		switch {
		case err == nil && info.Push:
			pushers = append(pushers, w)
		case err == nil:
			readers = append(readers, w)
		case errors.Is(err, forge.ErrAuth):
			o.logger.Warn("forge credential refused", "credential", c.ID, "host", host)
		case errors.Is(err, forge.ErrForbidden), errors.Is(err, forge.ErrNotFound):
			authed = true
		default:
			o.logger.Warn("forge repository read failed", "credential", c.ID, "host", host, "error", err)
			if otherErr == nil {
				otherErr, otherID = err, c.ID
			}
		}
	}
	if cands := append(pushers, readers...); len(cands) > 0 {
		return cands, nil
	}
	if otherErr != nil {
		return nil, forgeFailure(otherErr, otherID)
	}
	if authed {
		return nil, replyErr(forgeapi.CodeForgeAuthFailed, "no credential may read the repository")
	}
	return nil, replyErr(forgeapi.CodeForgeAuthFailed, "no credential authenticated to the forge")
}

// issueForbidden is the refusal when every candidate answered 403. One
// credential keeps the single-credential message. Several name each one.
func issueForbidden(ids []string) error {
	if len(ids) == 1 {
		return forgeFailure(forge.ErrForbidden, ids[0])
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = strconv.Quote(id)
	}
	return replyErr(forgeapi.CodeForgeWriteDenied, "credentials %s are forbidden; check the token scope and the repository access of each credential", strings.Join(quoted, ", "))
}

// Issue opens an issue for a Team agent. The operator shard that owns the
// Team answers it. The TeamAgent named by the request must be the accepted,
// enabled object of the Team, and its outputs must allow issues. Issue tries
// each credential that reads the repository, with the pushing credentials
// first. A forge 403 moves to the next credential. Any other error stops the
// attempt. The releaseNamespace is the core namespace, which the accept rule
// needs.
func (o *Ops) Issue(ctx context.Context, req forgeapi.IssueRequest, releaseNamespace string) forgeapi.IssueReply {
	fail := func(err error) forgeapi.IssueReply { return forgeapi.IssueReply{Status: asStatus(err)} }
	if req.Team == "" {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team is required"))
	}
	if title := strings.TrimSpace(req.Title); title == "" || utf8.RuneCountInString(title) > maxIssueTitle {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "title must be 1 to %d characters", maxIssueTitle))
	}
	ta, err := crdstore.Get[v1alpha1.TeamAgent](ctx, o.store, req.TeamAgent.Name, req.TeamAgent.Namespace)
	if apierrors.IsNotFound(err) {
		return fail(replyErr(forgeapi.CodeNotFound, "team agent %s/%s not found", req.TeamAgent.Namespace, req.TeamAgent.Name))
	}
	if err != nil {
		return fail(replyErr(forgeapi.CodeInternal, "read team agent: %v", err))
	}
	if string(ta.UID) != req.TeamAgent.UID {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team agent uid does not match the stored object"))
	}
	in, err := o.acceptInput(ctx, releaseNamespace)
	if err != nil {
		return fail(err)
	}
	verdict := accept.Decide(in)[types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}]
	if !verdict.Accepted {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team agent not accepted: %s", verdict.Reason))
	}
	if req.Team != ta.Spec.TeamRef.Name {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team does not match the team agent"))
	}
	if !accept.Enabled(ta) {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "team agent is disabled"))
	}
	out := ta.Spec.Outputs.Issues
	if out == nil {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "issue output is off"))
	}
	allowed := make(map[string]bool, len(out.Labels))
	for _, l := range out.Labels {
		allowed[l] = true
	}
	for _, l := range req.Labels {
		if !allowed[l] {
			return fail(replyErr(forgeapi.CodeInvalidRequest, "label %q is not allowed for this team agent", l))
		}
	}
	labels := append([]string(nil), out.Labels...)

	repo, err := forgeurl.NormalizeRepoURL(out.Repo)
	if err != nil {
		return fail(replyErr(forgeapi.CodeRepoURLInvalid, "%v", err))
	}
	creds, err := o.chooseCredential(ctx, req.Team, repo.Host)
	if err != nil {
		return fail(err)
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	cands, err := o.issueCandidates(ctx, repo.Host, fr, creds)
	if err != nil {
		return fail(err)
	}
	if string(cands[0].Kind) != out.Forge {
		return fail(replyErr(forgeapi.CodeForgeUnsupported, "repository forge is %s, not %s", cands[0].Kind, out.Forge))
	}

	body := req.Body + "\n\n---\nOpened by " + req.Actor + " in session " + req.SessionID + "."
	var forbidden []string
	for _, w := range cands {
		issue, err := w.Client.CreateIssue(ctx, fr, req.Title, body, labels)
		var le *forge.LabelError
		if errors.As(err, &le) {
			return fail(replyErr(forgeapi.CodeInvalidRequest, "label %q is not in the repository", le.Name))
		}
		if errors.Is(err, forge.ErrForbidden) {
			o.logger.Info("forge issue write forbidden; trying the next credential", "credential", w.CredentialID, "host", repo.Host)
			forbidden = append(forbidden, w.CredentialID)
			continue
		}
		if err != nil {
			return fail(forgeFailure(err, w.CredentialID))
		}
		o.logger.Info("forge issue opened", "credential", w.CredentialID, "host", repo.Host, "number", issue.Number, "forbiddenBefore", len(forbidden))
		return forgeapi.IssueReply{Status: forgeapi.Status{OK: true}, Number: issue.Number, URL: issue.URL}
	}
	return fail(issueForbidden(forbidden))
}

// acceptInput lists the TeamAgent and Team objects that accept.Decide needs.
func (o *Ops) acceptInput(ctx context.Context, releaseNamespace string) (accept.Input, error) {
	agents, err := crdstore.List[v1alpha1.TeamAgent](ctx, o.store, "", "")
	if err != nil {
		return accept.Input{}, replyErr(forgeapi.CodeInternal, "list team agents: %v", err)
	}
	teams, err := crdstore.List[v1alpha1.Team](ctx, o.store, "", "")
	if err != nil {
		return accept.Input{}, replyErr(forgeapi.CodeInternal, "list teams: %v", err)
	}
	in := accept.Input{ReleaseNamespace: releaseNamespace, Teams: make(map[string]*v1alpha1.Team, len(teams))}
	for _, a := range agents {
		in.TeamAgents = append(in.TeamAgents, *a)
	}
	for _, t := range teams {
		in.Teams[t.Name] = t
	}
	return in, nil
}
