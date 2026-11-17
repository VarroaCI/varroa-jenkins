package controller

import (
	"context"
	"log/slog"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// The next-boot CASC seed is the per-controller ConfigMap that the casc-seed
// init container copies into the CASC directory at each pod start. Jenkins
// applies it during init, so a document that Jenkins refuses stops the boot.
// The seed must therefore never carry a document that a live reload refused.
const (
	cascSeedRealmKey  = "realm.yaml"
	cascSeedConfigKey = "config.yaml"
	cascSeedRBACKey   = "rbac.yaml"
)

// cascSeedData answers the seed documents for a resolved bundle and an RBAC
// document. A nil bundle gives an empty config.yaml. An empty rbacYAML leaves
// rbac.yaml out. The config.yaml value never carries the credentials that
// buildDesiredStateCommand injects into a cell's JcascYaml.
func cascSeedData(resolved *bundle.MaterializedBundle, rbacYAML string) map[string]string {
	config := ""
	if resolved != nil {
		// Varroa owns authz and delivers it separately as rbac.yaml.
		config = injectProjectNamingStrategy(stripAuthorizationStrategy(resolved.JenkinsYAML))
	}
	data := map[string]string{
		cascSeedRealmKey:  generateRealmDocument(),
		cascSeedConfigKey: config,
	}
	if rbacYAML != "" {
		data[cascSeedRBACKey] = rbacYAML
	}
	return data
}

// cascSeedVerdict reads a mite result as a verdict on the seed documents of
// desired. ok is false when res gives no verdict: res is nil, res answers
// another desired state, or the mite applied nothing (a failed section with no
// error text, which the mite reports when Jenkins is not ready). On a verdict,
// refusal is nil when the live reload accepted every document of desired.
// Otherwise refusal holds the digest of each refused seed document.
func cascSeedVerdict(res *mitev1.DesiredStateResult, desired *mitev1.DesiredStateCommand, seed map[string]string) (refusal *v1alpha1.CascRefusal, ok bool) {
	if res == nil || desired == nil || res.AppliedHash == "" || res.AppliedHash != desired.DesiredStateHash {
		return nil, false
	}
	configFailed := desired.JcascYaml != "" && !res.ConfigSuccess
	rbacFailed := desired.RbacYaml != "" && !res.RbacSuccess
	configRefused := configFailed && res.ConfigError != ""
	rbacRefused := rbacFailed && res.RbacError != ""
	if !configRefused && !rbacRefused {
		if configFailed || rbacFailed {
			return nil, false
		}
		return nil, true
	}
	refusal = &v1alpha1.CascRefusal{}
	if configRefused {
		refusal.ConfigHash = sha256Hex([]byte(seed[cascSeedConfigKey]))
	}
	if rbacRefused {
		refusal.RBACHash = sha256Hex([]byte(seed[cascSeedRBACKey]))
	}
	return refusal, true
}

// cascSeedWithoutRefused answers the seed to write when refusal is set. A
// document of current whose digest matches a refused digest takes its value
// from existing, the seed that the ConfigMap holds now. When existing has no
// value for that document, the document is left out: a pod that boots without
// a document is better than a pod that cannot boot. Every other document,
// realm.yaml included, comes from current.
func cascSeedWithoutRefused(current, existing map[string]string, refusal *v1alpha1.CascRefusal) map[string]string {
	out := make(map[string]string, len(current))
	for k, v := range current {
		out[k] = v
	}
	if refusal == nil {
		return out
	}
	keep := func(key, refusedHash string) {
		v, present := out[key]
		if !present || refusedHash == "" || sha256Hex([]byte(v)) != refusedHash {
			return
		}
		if old, had := existing[key]; had {
			out[key] = old
			return
		}
		delete(out, key)
	}
	keep(cascSeedConfigKey, refusal.ConfigHash)
	keep(cascSeedRBACKey, refusal.RBACHash)
	return out
}

// advanceCascSeed moves the next-boot seed after a mite result for the
// desired state of this tick. An accepted result writes the seed of desired
// and clears status.cascRefusal. A refused result records the refused
// documents in status.cascRefusal and writes a seed that keeps the existing
// value of each refused document. A result that gives no verdict changes
// nothing. A seed write fault is not fatal: the result of the next push
// writes the seed again.
func (r *Reconciler) advanceCascSeed(ctx context.Context, cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle,
	desired *mitev1.DesiredStateCommand, res *mitev1.DesiredStateResult, logger *slog.Logger) {
	seed := cascSeedData(resolved, desired.RbacYaml)
	refusal, ok := cascSeedVerdict(res, desired, seed)
	if !ok {
		return
	}
	cr.Status.CascRefusal = refusal
	name := cascConfigMapName(controllerPrefix(cr))
	if refusal != nil {
		existing, err := r.client.GetConfigMap(ctx, name, cr.Namespace)
		if err != nil {
			logger.Warn("read CASC seed failed; the seed keeps its content", "error", err)
			return
		}
		seed = cascSeedWithoutRefused(seed, existing, refusal)
	}
	if err := r.client.CreateOrUpdateConfigMap(ctx, name, cr.Namespace, seed); err != nil {
		logger.Warn("failed to update CASC ConfigMap", "error", err)
	}
}
