package forgeops

import (
	"context"
	"regexp"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// forgeLoginPattern is the shape of a GitHub login. The check runs before the
// login reaches a forge URL.
var forgeLoginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// Permission reports the repository permission of one forge user. It reads
// through the tenant credential that can read the repository. A client that
// does not implement forge.PermissionReader answers forge_unsupported.
func (o *Ops) Permission(ctx context.Context, req forgeapi.PermissionRequest) forgeapi.PermissionReply {
	fail := func(err error) forgeapi.PermissionReply { return forgeapi.PermissionReply{Status: asStatus(err)} }
	if req.Tenant == "" {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "tenant is required"))
	}
	if !forgeLoginPattern.MatchString(req.Login) {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "login is not a forge user name"))
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return fail(replyErr(forgeapi.CodeRepoURLInvalid, "%v", err))
	}
	creds, err := o.chooseCredential(ctx, req.Tenant, repo.Host)
	if err != nil {
		return fail(err)
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}
	w, err := o.pickReader(ctx, repo.Host, fr, creds)
	if err != nil {
		return fail(err)
	}
	reader, ok := w.Client.(forge.PermissionReader)
	if !ok {
		return fail(replyErr(forgeapi.CodeForgeUnsupported, "forge %q cannot report a collaborator permission", w.Kind))
	}
	permission, err := reader.CollaboratorPermission(ctx, fr, req.Login)
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	return forgeapi.PermissionReply{Status: forgeapi.Status{OK: true}, Permission: permission}
}
