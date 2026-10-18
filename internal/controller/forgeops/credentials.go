package forgeops

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// chooseCredential returns the Team's tenant credentials that may reach host,
// sorted by id. A credential qualifies when it has a known kind, a token value,
// and a varroa.dev/allowed-hosts annotation that lists host.
func (o *Ops) chooseCredential(ctx context.Context, team, host string) ([]Credential, error) {
	t, err := crdstore.Get[v1alpha1.Team](ctx, o.store, team, "")
	if apierrors.IsNotFound(err) {
		return nil, replyErr(forgeapi.CodeNoMatchingCredential, "team %q has no pipelines agent namespace", team)
	}
	if err != nil {
		return nil, replyErr(forgeapi.CodeInternal, "read team %q failed", team)
	}
	if t.Spec.Pipelines == nil || t.Spec.Pipelines.AgentNamespace == "" {
		return nil, replyErr(forgeapi.CodeNoMatchingCredential, "team %q has no pipelines agent namespace", team)
	}
	ns := t.Spec.Pipelines.AgentNamespace

	records, err := o.secrets.ListSecrets(ctx, ns, v1alpha1.TenantCredentialSelector)
	if err != nil {
		return nil, replyErr(forgeapi.CodeInternal, "list tenant credentials of team %q failed", team)
	}
	var out []Credential
	for _, rec := range records {
		name := string(rec["_name"])
		if name == "" {
			continue
		}
		ann, err := o.secrets.GetSecretAnnotations(ctx, name, ns)
		if err != nil {
			continue
		}
		c, ok := credentialFrom(name, ann, rec, host)
		if ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, replyErr(forgeapi.CodeNoMatchingCredential, "no tenant credential of team %q lists host %q", team, host)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].SecretName < out[j].SecretName
	})
	return out, nil
}

// credentialFrom applies the kind, token, and host rules to one Secret.
func credentialFrom(name string, ann map[string]string, data map[string][]byte, host string) (Credential, bool) {
	id := ann[v1alpha1.CredentialIDAnnotation]
	if id == "" {
		return Credential{}, false
	}
	var key string
	switch ann[v1alpha1.CredentialKindAnnotation] {
	case v1alpha1.CredentialKindUsernamePassword:
		key = "password"
	case v1alpha1.CredentialKindSecretText:
		key = "secret"
	default:
		return Credential{}, false
	}
	tok := data[key]
	if len(tok) == 0 {
		return Credential{}, false
	}
	if err := bundle.CheckHostAllowed(ann, host); err != nil {
		return Credential{}, false
	}
	return Credential{ID: id, SecretName: name, Token: forge.NewToken(tok)}, true
}
