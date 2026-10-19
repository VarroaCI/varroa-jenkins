package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items/lint"
)

// assembleCellState is what assembleCellContent computes for one cell tick:
// the merged items.yaml with no tenant credential, the tenant clouds to
// append to jenkins.clouds, the SYSTEM-scope credentials those clouds need,
// each tenant's pipeline-template fragment, each tenant folder's projected
// credentials, and the tenantWarnings to record. The merged items name
// each held tenant in their retain list.
type assembleCellState struct {
	ItemsYAML         string
	Clouds            []bundle.TenantCloud
	CloudCredentials  []tenantCloudCredential
	TemplateFragments []bundle.CellTemplateFragment
	FolderCredentials []bundle.CellFolderCredentials
	Warnings          []v1alpha1.TenantWarning
}

// assembleCellContent runs the tenant items pipeline (design D7) plus
// tenant credential, folder, and cloud assembly (D8, D10, D11) for a cell.
// teams is task 8.11a's placed-tenant listing, read once per tick. coreSet
// is the cell's resolved plugin set. templateBase is the cell's
// jenkins.yaml with the cell bundle's own pipeline templates in it; a
// tenant fragment must load against it.
//
// All projected credentials of the cell share one byte budget. Tenants are
// taken in name order, so the same inputs skip the same credentials.
//
// Each non-draining tenant's items pass every check on their own. A tenant
// that fails one is held: its folder is absent from the merged items and
// named in the manifest's retain list, so the mite keeps the folder as it
// is. The other tenants still get their push. A hold never deletes a job,
// because an absent folder in a sync manifest is otherwise deleted.
//
// A read error for a non-draining tenant's bundle fails the assembly. It
// is a fault of the API read, not of the tenant content, and a hold would
// push the tenant's cloud with no pod template and drop its templates.
func (r *Reconciler) assembleCellContent(ctx context.Context, cr *v1alpha1.Controller, teams []*v1alpha1.Team, coreSet []pluginlock.PluginEntry, templateBase string) (assembleCellState, error) {
	var (
		manifests []*items.Manifest
		clouds    []bundle.TenantCloud
		creds     []tenantCloudCredential
		fragments []bundle.CellTemplateFragment
		folderCrd []bundle.CellFolderCredentials
		held      []string
		warnings  []v1alpha1.TenantWarning
	)

	hold := func(team string, fault error) {
		held = append(held, team)
		warnings = append(warnings, v1alpha1.TenantWarning{
			Tenant:  team,
			Reason:  v1alpha1.TeamReasonItemsHeld,
			Message: fmt.Sprintf("tenant folder held: %v", fault),
		})
		if r.Logger != nil {
			r.Logger.Warn("cell content: tenant items fail a check; holding the tenant folder",
				"team", team, "error", fault)
		}
	}

	ordered := slices.Clone(teams)
	slices.SortFunc(ordered, func(a, b *v1alpha1.Team) int { return strings.Compare(a.Name, b.Name) })
	budget := newCellCredentialBudget(cellCredentialBudgetBytes)

	for _, team := range ordered {
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
		var notReady error
		if errors.Is(err, errTenantBundleNotReady) {
			notReady, err = err, nil
		}
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
		// provision an agent. A draining tenant is never held, because a
		// held folder would never drain.
		if !draining {
			fragment, templateFindings := bundle.LintTenantTemplates(data["pipeline-templates.yaml"])
			// Two tenant folders never overlap, so two tenant fragments never
			// conflict: each fragment loads against the cell's own base only.
			_, fragErr := bundle.InjectPipelineTemplates(templateBase, fragment, team.Name)
			rerooted, err := materializeTenantManifest(team.Name, data, coreSet)
			if notReady != nil {
				err = notReady
			}
			if err == nil && fragErr != nil {
				err = fmt.Errorf("pipeline-templates.yaml: %w", fragErr)
			}
			if err == nil && len(templateFindings) > 0 {
				err = fmt.Errorf("pipeline template lint failed: %s", lint.Summary(templateFindings))
			}
			if err != nil {
				hold(team.Name, err)
			} else {
				folder := &rerooted.Items[0]

				// Projected onto a separate folder item and carried: a value
				// from a tenant Secret must not reach ResolveVars or the
				// unresolved-variable check, which run on ItemsYAML after this.
				projected := &items.Item{Kind: folder.Kind, Name: folder.Name}
				warnings = append(warnings, r.projectTenantCredentials(ctx, team.Name, agentNamespace, projected)...)
				prop := folderCredentialsPropertyOf(projected)
				charged, budgetWarnings := budget.trim(team.Name, prop)
				// A library names a credential that its folder cannot see.
				// That tenant is held, and none of its content is carried,
				// so its bytes return to the budget and its skips go unreported.
				if err := checkFolderLibraryCredentials(rerooted, prop.FolderCredentials); err != nil {
					budget.refund(charged)
					hold(team.Name, err)
				} else {
					warnings = append(warnings, budgetWarnings...)
					if len(prop.FolderCredentials) > 0 {
						folderCrd = append(folderCrd, bundle.CellFolderCredentials{Folder: folder.Name, Entries: prop.FolderCredentials})
					}

					folder.Properties = append(folder.Properties, items.Property{
						Kubernetes: &items.KubernetesFolderProperty{PermittedClouds: []string{team.Name}},
					})

					manifests = append(manifests, rerooted)
				}
			}

			// A tenant held for its items keeps its template fragment, so
			// the held jobs can still name a template of the tenant. A
			// fragment that does not load is dropped, because it would
			// fail the cell for every tenant. A template that fails the
			// lint is dropped from the fragment; the tenant's other
			// templates stay.
			if fragErr == nil && strings.TrimSpace(fragment) != "" {
				fragments = append(fragments, bundle.CellTemplateFragment{
					Folder:        team.Name,
					TemplatesYAML: fragment,
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
	sort.Strings(held)
	merged.Retain = held

	out, err := yaml.Marshal(merged)
	if err != nil {
		return assembleCellState{}, fmt.Errorf("marshal cell items: %w", err)
	}

	return assembleCellState{
		ItemsYAML:         string(out),
		Clouds:            clouds,
		CloudCredentials:  creds,
		TemplateFragments: fragments,
		FolderCredentials: folderCrd,
		Warnings:          warnings,
	}, nil
}

// errTenantBundleNotReady reports a tenant whose composedBundleRef names a
// bundle that is missing or has not materialized yet. A non-draining
// tenant is held for it: an empty folder in a sync manifest would delete
// every job the tenant already has on the cell.
var errTenantBundleNotReady = errors.New("tenant bundle is missing or not materialized")

// readTenantBundleContent reads a tenant's materialized content ConfigMap
// (design D7 step 1, tenant-isolation). An unset composedBundleRef returns
// nil data and no error, so the tenant gets an empty folder and no
// template. A missing bundle, or a bundle that has not materialized yet,
// returns nil data and an error that wraps errTenantBundleNotReady. Any
// other read error, of the bundle or of its ConfigMap, returns an error
// that does not wrap it.
func (r *Reconciler) readTenantBundleContent(ctx context.Context, team *v1alpha1.Team) (map[string]string, error) {
	ref := team.Spec.Pipelines.ComposedBundleRef
	if ref == nil {
		return nil, nil
	}
	name := ref.Namespace + "/" + ref.Name

	cb, err := r.getComposedBundle(ctx, ref.Name, ref.Namespace)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s is not found", errTenantBundleNotReady, name)
	}
	if err != nil {
		return nil, err
	}
	if cb.Status.ContentRef == "" {
		return nil, fmt.Errorf("%w: %s has no content yet", errTenantBundleNotReady, name)
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
	lift := liftTenantPodTemplates(team, agentNamespace, data["jenkins.yaml"], bundle.ParseVariables(data["variables.yaml"]))
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
	// A declared top-level item key that a cell does not apply is named
	// here. An undefined key or root holds the tenant instead, in
	// materializeTenantManifest. A parse fault also holds it there.
	if _, declared, err := bundle.SplitDroppedItemKeys(data); err == nil {
		for _, d := range declared {
			if d.Key == "root" {
				continue
			}
			warnings = append(warnings, v1alpha1.TenantWarning{
				Tenant:  team,
				Reason:  v1alpha1.TeamReasonBundleKeysIgnored,
				Message: fmt.Sprintf("tenant item manifest key %q from %s is ignored on a cell", d.Key, d.Source),
			})
		}
	}
	return lift.Templates, warnings
}

// materializeTenantManifest returns a tenant's items.yaml re-rooted under
// its folder, or the first check it fails. The item checks come from
// bundle.TenantItemsManifest: the dropped item keys, a strict parse of
// the tenant's own variables, and the cell lint rules. The plugin-set
// checks run here only, because they need the cell's coreSet. Nil data
// gives an empty folder.
func materializeTenantManifest(team string, data map[string]string, coreSet []pluginlock.PluginEntry) (*items.Manifest, error) {
	rerooted, err := bundle.TenantItemsManifest(team, data)
	if err != nil {
		return nil, err
	}
	if err := checkBranchBuildStrategiesManifest(rerooted, coreSet); err != nil {
		return nil, err
	}
	if err := checkFolderLibraryPluginsManifest(rerooted, coreSet); err != nil {
		return nil, err
	}
	if err := checkGiteaPluginManifest(rerooted, coreSet); err != nil {
		return nil, err
	}
	return rerooted, nil
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

// recordCellTenantWarnings rebuilds cr.Status.Cell.TenantWarnings' entries
// for reasons BundleKeysIgnored, PodTemplateSkipped, CredentialSkipped, and
// ItemsHeld
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
	cr.Status.Cell.TenantWarnings = replaceTenantWarnings(cr.Status.Cell.TenantWarnings,
		v1alpha1.TeamReasonItemsHeld, filterTenantWarnings(fresh, v1alpha1.TeamReasonItemsHeld))
}

// dropHeldTenants removes every tenant the cell holds from
// status.cell.appliedTenants. updateCellInstance calls it when the cell's
// pod changes: the new pod starts with an empty home, so a held folder is
// gone, and appliedTenantsAfterItemsApply would otherwise keep reporting
// it applied.
func dropHeldTenants(cell *v1alpha1.CellStatus) {
	held := heldTenants(cell)
	if len(held) == 0 {
		return
	}
	kept := cell.AppliedTenants[:0:0]
	for _, tenant := range cell.AppliedTenants {
		if !held[tenant] {
			kept = append(kept, tenant)
		}
	}
	cell.AppliedTenants = sortedTenantCopy(kept)
}

// heldTenants answers the tenants that have an ItemsHeld entry.
func heldTenants(cell *v1alpha1.CellStatus) map[string]bool {
	held := make(map[string]bool)
	for _, w := range cell.TenantWarnings {
		if w.Reason == v1alpha1.TeamReasonItemsHeld {
			held[w.Tenant] = true
		}
	}
	return held
}

// appliedTenantsAfterItemsApply answers status.cell.appliedTenants after an
// items apply that walked every item. The walk can refuse some items and
// still complete. A held tenant keeps its previous state. A held tenant that
// was applied still has its folder on the cell, so it must not read as
// drained. A held tenant that was never applied has no folder, so it must not
// read as applied. The held set is the ItemsHeld entries, which the same
// resolve that produced the desired state wrote.
//
// A tenant that owns a refused item keeps its previous state too. Its earlier
// folder stays on the cell, and a new folder is not complete. A refused path
// that no tenant owns changes no tenant.
func appliedTenantsAfterItemsApply(cell *v1alpha1.CellStatus, refusedPaths []string) []string {
	kept := heldTenants(cell)
	for tenant := range partitionJobPathsByTenant(refusedPaths, cell.Tenants) {
		kept[tenant] = true
	}
	applied := make([]string, 0, len(cell.Tenants))
	for _, tenant := range cell.Tenants {
		if !kept[tenant] || slices.Contains(cell.AppliedTenants, tenant) {
			applied = append(applied, tenant)
		}
	}
	return sortedTenantCopy(applied)
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
