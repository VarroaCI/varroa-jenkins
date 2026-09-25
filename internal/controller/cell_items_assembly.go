package controller

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items/lint"
)

// cellLintRules is the fixed rule set a cell's merged tenant manifest must
// pass before the operator pushes it (design D9).
var cellLintRules = []lint.Rule{
	lint.NoPollSCMRule{},
	lint.NoGlobalCredentialsRule{},
	lint.NoControllerConfigDependencyRule{},
}

// assembleCellState is what assembleCellContent computes for one cell tick:
// the merged items.yaml ready to send as-is, the tenant clouds to append to
// jenkins.clouds, the SYSTEM-scope credentials those clouds need, each
// tenant's pipeline-template fragment, and the tenantWarnings to record.
type assembleCellState struct {
	ItemsYAML         string
	Clouds            []bundle.TenantCloud
	CloudCredentials  []tenantCloudCredential
	TemplateFragments []bundle.CellTemplateFragment
	Warnings          []v1alpha1.TenantWarning
}

// assembleCellContent runs the four-step tenant items pipeline (design D7)
// plus tenant credential, folder, and cloud assembly (D8, D10, D11) for a
// cell. teams is task 8.11a's placed-tenant listing, read once per tick.
// controllerEndpoint is the cell's own resolved varroa_controller_endpoint.
// A tenant cloud's jenkinsUrl no longer reads it: each cloud names the
// tenant's own agent Service instead.
func (r *Reconciler) assembleCellContent(ctx context.Context, cr *v1alpha1.Controller, teams []*v1alpha1.Team, controllerEndpoint string) (assembleCellState, error) {
	_ = controllerEndpoint
	var (
		manifests []*items.Manifest
		clouds    []bundle.TenantCloud
		creds     []tenantCloudCredential
		fragments []bundle.CellTemplateFragment
		warnings  []v1alpha1.TenantWarning
	)

	for _, team := range teams {
		if team.Spec.Pipelines == nil {
			// A Team can carry a stale entry in status.cell.tenants for one
			// tick after spec.pipelines is cleared; teardown is what
			// removes it from the list, so this is a skip, not a failure.
			continue
		}
		agentNamespace := team.Spec.Pipelines.AgentNamespace
		draining := tenantIsDrainingFromCell(team, cr)

		// A draining tenant still reads its bundle, because its cloud keeps
		// the lifted pod templates. A read fault for a draining tenant
		// lifts nothing and does not fail the cell.
		data, err := r.readTenantBundleContent(ctx, team)
		if err != nil {
			if !draining {
				return assembleCellState{}, fmt.Errorf("tenant %q: %w", team.Name, err)
			}
			if r.Logger != nil {
				r.Logger.Warn("cell content: draining tenant bundle unreadable; lifting no pod template",
					"team", team.Name, "error", err)
			}
			data = nil
		}
		lifted, bundleWarnings := tenantBundleCloudContent(team.Name, agentNamespace, data)
		warnings = append(warnings, bundleWarnings...)

		// A draining tenant loses only its folder here. That is what
		// converges the voluntary move's idle test (design D4): no new
		// build can start, because the job is gone. Its cloud and cloud
		// credential stay until the tenant leaves status.cell.tenants, so
		// a build already running when the folder disappeared can still
		// provision an agent.
		if !draining {
			tenantManifest, tenantTemplatesYAML, err := materializeTenantManifest(data)
			if err != nil {
				return assembleCellState{}, fmt.Errorf("tenant %q: %w", team.Name, err)
			}

			rerooted, err := items.ReRoot(tenantManifest, team.Name)
			if err != nil {
				return assembleCellState{}, err
			}
			folder := &rerooted.Items[0]

			warnings = append(warnings, r.projectTenantCredentials(ctx, team.Name, agentNamespace, folder)...)

			folder.Properties = append(folder.Properties, items.Property{
				Kubernetes: &items.KubernetesFolderProperty{PermittedClouds: []string{team.Name}},
			})

			manifests = append(manifests, rerooted)

			if strings.TrimSpace(tenantTemplatesYAML) != "" {
				fragments = append(fragments, bundle.CellTemplateFragment{
					Folder:        team.Name,
					TemplatesYAML: tenantTemplatesYAML,
				})
			}
		}

		// The cloud names the agent Service of the slot this cell holds
		// for the tenant. A standby holds no slot, so its cloud names
		// slot 0's Service: the cloud must exist before a promotion flips
		// the selector, or a first resumed build would find no cloud to
		// provision from. A standby never launches an agent, because the
		// claim fence answers NOT_HOME for every job.
		agentEndpoint := tenantAgentEndpoint(team.Name, cr.Namespace, tenantSlotOnCell(team, cr))
		clouds = append(clouds, tenantCloudFor(team.Name, agentNamespace, agentEndpoint, lifted))
		if cred := r.tenantCloudCredentialFor(ctx, team.Name, agentNamespace); cred != nil {
			creds = append(creds, *cred)
		}
	}

	merged, err := items.Merge(manifests)
	if err != nil {
		return assembleCellState{}, err
	}

	if findings := lint.Run(merged, cellLintRules); len(findings) > 0 {
		return assembleCellState{}, fmt.Errorf("cell items lint failed: %s", findingsSummary(findings))
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return assembleCellState{}, fmt.Errorf("marshal cell items: %w", err)
	}

	return assembleCellState{
		ItemsYAML:         string(out),
		Clouds:            clouds,
		CloudCredentials:  creds,
		TemplateFragments: fragments,
		Warnings:          warnings,
	}, nil
}

// readTenantBundleContent reads a tenant's materialized content ConfigMap
// (design D7 step 1, tenant-isolation). An unset composedBundleRef, a
// missing bundle, or a bundle that has not materialized yet returns nil
// data and no error, so the tenant gets an empty folder and no template
// rather than failing the whole cell.
func (r *Reconciler) readTenantBundleContent(ctx context.Context, team *v1alpha1.Team) (map[string]string, error) {
	ref := team.Spec.Pipelines.ComposedBundleRef
	if ref == nil {
		return nil, nil
	}

	cb, err := r.getComposedBundle(ctx, ref.Name, ref.Namespace)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("cell content: tenant bundle not found; using an empty folder",
				"team", team.Name, "bundle", ref.Namespace+"/"+ref.Name, "error", err)
		}
		return nil, nil
	}
	if cb.Status.ContentRef == "" {
		return nil, nil
	}

	data, err := r.client.GetConfigMap(ctx, cb.Status.ContentRef, cb.Namespace)
	if err != nil {
		return nil, fmt.Errorf("read tenant bundle content configmap %s: %w", cb.Status.ContentRef, err)
	}
	return data, nil
}

// tenantBundleCloudContent lifts the pod templates of a tenant's
// jenkins.yaml for its cloud, with the tenant's own variables.yaml, and
// returns the lift's warnings. It also records a BundleKeysIgnored warning
// for a non-empty plugins.yaml or rbac.yaml, which a cell never applies.
// It runs for a draining tenant too.
func tenantBundleCloudContent(team, agentNamespace string, data map[string]string) ([]bundle.TenantCloudTemplate, []v1alpha1.TenantWarning) {
	lift := liftTenantPodTemplates(team, agentNamespace, data["jenkins.yaml"], parseBundleVariables(data["variables.yaml"]))
	warnings := lift.Warnings
	for _, key := range []string{"plugins.yaml", "rbac.yaml"} {
		if strings.TrimSpace(data[key]) != "" {
			warnings = append(warnings, v1alpha1.TenantWarning{
				Tenant:  team,
				Reason:  v1alpha1.TeamReasonBundleKeysIgnored,
				Message: fmt.Sprintf("tenant bundle key %q is ignored on a cell", key),
			})
		}
	}
	return lift.Templates, warnings
}

// materializeTenantManifest parses a tenant's items.yaml and returns it
// with the tenant's pipeline-templates.yaml. Nil data gives an empty
// manifest and no template.
func materializeTenantManifest(data map[string]string) (*items.Manifest, string, error) {
	manifest, err := items.Parse(data["items.yaml"])
	if err != nil {
		return nil, "", fmt.Errorf("parse tenant items.yaml: %w", err)
	}
	return manifest, data["pipeline-templates.yaml"], nil
}

// tenantCloudCredentialFor reads the tenant agent's ServiceAccount token
// Secret (design D7/D11) and returns the SYSTEM-scope credential to inject
// for it, or nil while the token controller has not populated the token
// key yet (a transient state right after placement, not an error).
func (r *Reconciler) tenantCloudCredentialFor(ctx context.Context, team, agentNamespace string) *tenantCloudCredential {
	data, err := r.client.GetSecret(ctx, tenantAgentTokenSecretName(team), agentNamespace)
	if err != nil || len(data["token"]) == 0 {
		return nil
	}
	return &tenantCloudCredential{ID: tenantCloudCredentialID(team), Secret: string(data["token"])}
}

// findingsSummary renders lint findings as one line per finding, each
// naming its rule id, item path, and message.
func findingsSummary(findings []lint.Finding) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("%s at %q: %s", f.Rule, f.Path, f.Message))
	}
	return strings.Join(lines, "; ")
}

// recordCellTenantWarnings rebuilds cr.Status.Cell.TenantWarnings' entries
// for reasons BundleKeysIgnored, PodTemplateSkipped, and CredentialSkipped
// from fresh, leaving every other reason's entries (e.g.
// HibernationIgnored) untouched (design D4: "Both reconcilers rebuild their
// whole list each tick").
func recordCellTenantWarnings(cr *v1alpha1.Controller, fresh []v1alpha1.TenantWarning) {
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	cr.Status.Cell.TenantWarnings = replaceTenantWarnings(cr.Status.Cell.TenantWarnings,
		v1alpha1.TeamReasonBundleKeysIgnored, filterTenantWarnings(fresh, v1alpha1.TeamReasonBundleKeysIgnored))
	cr.Status.Cell.TenantWarnings = replaceTenantWarnings(cr.Status.Cell.TenantWarnings,
		v1alpha1.TeamReasonPodTemplateSkipped, filterTenantWarnings(fresh, v1alpha1.TeamReasonPodTemplateSkipped))
	cr.Status.Cell.TenantWarnings = replaceTenantWarnings(cr.Status.Cell.TenantWarnings,
		v1alpha1.TeamReasonCredentialSkipped, filterTenantWarnings(fresh, v1alpha1.TeamReasonCredentialSkipped))
}

// filterTenantWarnings returns the subset of in with the given reason.
func filterTenantWarnings(in []v1alpha1.TenantWarning, reason string) []v1alpha1.TenantWarning {
	var out []v1alpha1.TenantWarning
	for _, w := range in {
		if w.Reason == reason {
			out = append(out, w)
		}
	}
	return out
}
