package forgeops

import (
	"context"
	"errors"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// RepositoryGone reports whether the forge confirms that repoURL no longer
// exists. Only one answer is a confirmation: a 404 from a Gitea or Forgejo
// credential that sees every repository of the owner, and that proves it
// reads private repositories of the owner. Every other answer
// keeps the repository present or unknown, and the result is false. A GitHub
// host always answers false, because its 404 also answers for a private
// repository the credential cannot see. No credential, an unsupported forge,
// a refused URL, and every error answer false too.
func (o *Ops) RepositoryGone(ctx context.Context, team, repoURL string) bool {
	repo, err := forgeurl.NormalizeRepoURL(repoURL)
	if err != nil {
		o.logger.Debug("repository probe skipped: repo URL refused", "team", team, "error", err)
		return false
	}
	creds, err := o.chooseCredential(ctx, team, repo.Host)
	if err != nil {
		o.logger.Debug("repository probe skipped: no tenant credential", "team", team, "host", repo.Host)
		return false
	}
	kind, err := o.detect(ctx, o.http, repo.Host)
	if err != nil {
		o.logger.Debug("repository probe skipped: forge not detected", "team", team, "host", repo.Host)
		return false
	}
	if kind != forge.KindGitea {
		o.logger.Debug("repository probe unknown: forge kind cannot confirm a gone repository", "team", team, "host", repo.Host, "kind", string(kind))
		return false
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	for _, c := range creds {
		cl := o.newClient(kind, repo.Host, c.Token, o.http)
		if cl == nil {
			continue
		}
		_, err := cl.Repository(ctx, fr)
		switch {
		case err == nil:
			o.logger.Debug("repository probe: repository present", "team", team, "host", repo.Host, "credential", c.ID)
			return false
		case errors.Is(err, forge.ErrNotFound):
			scope, ok := cl.(forge.OwnerScope)
			if !ok {
				continue
			}
			all, err := scope.SeesAllOwnerRepos(ctx, repo.Owner)
			if err != nil || !all {
				o.logger.Debug("repository probe: 404 without owner scope", "team", team, "host", repo.Host, "credential", c.ID)
				continue
			}
			o.logger.Info("repository probe: repository gone", "team", team, "host", repo.Host, "credential", c.ID)
			return true
		default:
			o.logger.Debug("repository probe: credential could not answer", "team", team, "host", repo.Host, "credential", c.ID)
		}
	}
	return false
}
