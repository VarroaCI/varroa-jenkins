package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// resolveRuntimeUpdateSiteURL returns the URL of the runtime update site that
// the Jenkins controller reads. The first match wins:
//
//  1. ProvisioningDefaults.spec.pluginUpdateCenterURL. A value that ends in
//     update-center.actual.json is rewritten to update-center.json.
//  2. The in-cluster update center, when its base URL is set.
//  3. The Jenkins upstream, when jenkins.upstreamURL is set.
//
// An empty string means no site. The caller writes an empty site list.
// The path segment update-center.json is required: Jenkins reads
// tool-installer metadata only from a site URL that holds this name.
// plugins-init reads the .actual.json form, so the explicit value can carry it.
func resolveRuntimeUpdateSiteURL(defaults *v1alpha1.ProvisioningDefaults, ucBaseURL, jenkinsUpstreamURL string) string {
	if defaults != nil && defaults.Spec.PluginUpdateCenterURL != "" {
		explicit := defaults.Spec.PluginUpdateCenterURL
		if base, ok := strings.CutSuffix(explicit, "update-center.actual.json"); ok {
			return base + "update-center.json"
		}
		return explicit
	}
	if base := strings.TrimRight(ucBaseURL, "/"); base != "" {
		return base + "/update-center.json"
	}
	if base := strings.TrimRight(jenkinsUpstreamURL, "/"); base != "" {
		return base + "/update-center.json"
	}
	return ""
}

// applyRuntimeUpdateSite writes jenkins.updateCenter into the resolved Jenkins
// YAML. The operator is the only writer of that key. It replaces any value the
// bundle set, because the update site decides where Jenkins fetches plugin
// metadata.
func (r *Reconciler) applyRuntimeUpdateSite(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle) error {
	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read provisioning defaults: %w", err)
	}
	url := resolveRuntimeUpdateSiteURL(defaults, r.ucBaseURL, r.jenkinsUpstreamURL)
	out, overrode, err := bundle.InjectUpdateSites(mat.JenkinsYAML, url)
	if err != nil {
		return err
	}
	if overrode {
		r.Logger.Info("overrode bundle-provided update center sites", "controller", cr.Name, "namespace", cr.Namespace)
	}
	mat.JenkinsYAML = out
	return nil
}
