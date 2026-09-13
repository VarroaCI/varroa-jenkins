package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// composedBundleContent reads one ComposedBundle's content ConfigMap by
// name and namespace. It returns a nil map, with no error, when the
// bundle or its ConfigMap cannot be found: a tenant with no catalog
// template still serves record-derived data alone.
func (s *Server) composedBundleContent(ctx context.Context, bundleName, bundleNamespace string) (map[string]string, error) {
	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, s.deps.Store, bundleName, bundleNamespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if cb.Status.ContentRef == "" {
		return nil, nil
	}

	cm, err := s.deps.Client.GetConfigMap(ctx, cb.Status.ContentRef, bundleNamespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return cm, nil
}

// controllerBundleContent reads ref's controller's composed bundle content
// ConfigMap. It returns a nil cr and a nil map, with no error, when the
// controller, its bundle, or the ConfigMap cannot be found: a tenant with
// no catalog template still serves record-derived data alone. Design D1's
// cell mode names a controller ref.Controller does not yet identify as a
// Controller CR, which the same not-found path also covers.
func (s *Server) controllerBundleContent(ctx context.Context, ref TenantRef) (*v1alpha1.Controller, map[string]string, error) {
	if ref.Controller == "" {
		return nil, nil, nil
	}
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, ref.Controller, ref.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	bundleName := v1alpha1.StarterBundleName
	bundleNamespace := s.deps.OperatorNamespace
	if cr.Spec.ComposedBundleRef != nil {
		bundleName = cr.Spec.ComposedBundleRef.Name
		bundleNamespace = cr.Spec.ComposedBundleRef.Namespace
		if bundleNamespace == "" {
			bundleNamespace = cr.Namespace
		}
	}

	cm, err := s.composedBundleContent(ctx, bundleName, bundleNamespace)
	if err != nil {
		return cr, nil, err
	}
	return cr, cm, nil
}

// teamPipelineBundleContent reads a placed Team's own composed bundle
// content ConfigMap, named by Team.spec.pipelines.composedBundleRef. It
// returns a nil map, with no error, when the Team, its pipelines spec,
// its bundle ref, or the bundle's ConfigMap cannot be found: a tenant
// with no catalog template still serves record-derived data alone.
func (s *Server) teamPipelineBundleContent(ctx context.Context, ref TenantRef) (map[string]string, error) {
	if ref.Team == "" {
		return nil, nil
	}
	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, ref.Team, "")
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if team.Spec.Pipelines == nil || team.Spec.Pipelines.ComposedBundleRef == nil {
		return nil, nil
	}
	bundleRef := team.Spec.Pipelines.ComposedBundleRef
	return s.composedBundleContent(ctx, bundleRef.Name, bundleRef.Namespace)
}

// tenantController returns ref's controller, or nil when it does not
// exist. A read error other than not-found is returned.
func (s *Server) tenantController(ctx context.Context, ref TenantRef) (*v1alpha1.Controller, error) {
	if ref.Controller == "" {
		return nil, nil
	}
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, ref.Controller, ref.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return cr, nil
}

// controllerItemManifest reads and parses the items.yaml that declares
// ref's jobs. A placed Team (ref.Folder set) reads its own tenant bundle
// and re-roots the manifest under the Team folder, which is how the cell
// assembles it; the cell's bundle never carries a tenant job. A synthetic
// single-controller tenant (ref.Folder empty) reads its controller's
// bundle. It returns a nil manifest, with no error, when the controller,
// the bundle, or an items.yaml key cannot be found: a tenant with no
// declared item still serves record-derived data alone.
func (s *Server) controllerItemManifest(ctx context.Context, ref TenantRef) (*v1alpha1.Controller, *items.Manifest, error) {
	var (
		cr  *v1alpha1.Controller
		cm  map[string]string
		err error
	)
	if ref.Folder != "" {
		// A placed Team never reads the cell's bundle; the controller is
		// fetched only for its version.
		cr, err = s.tenantController(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		cm, err = s.teamPipelineBundleContent(ctx, ref)
	} else {
		cr, cm, err = s.controllerBundleContent(ctx, ref)
	}
	if err != nil {
		return cr, nil, err
	}
	if cm == nil {
		return cr, nil, nil
	}
	content, ok := cm["items.yaml"]
	if !ok || content == "" {
		return cr, nil, nil
	}
	manifest, err := items.Parse(content)
	if err != nil {
		// An items.yaml that fails to parse leaves the pipeline surface
		// without a definition rather than failing the read: the
		// bundle-validation gate is the place that rejects a bad manifest.
		return cr, nil, nil //nolint:nilerr // best-effort enrichment, see comment above
	}
	if ref.Folder != "" {
		manifest, err = items.ReRoot(manifest, ref.Folder)
		if err != nil {
			return cr, nil, nil //nolint:nilerr // a tenant manifest that names its own root is rejected upstream
		}
	}
	return cr, manifest, nil
}

// controllerPipelineTemplates reads and parses the composed bundle
// pipeline-templates.yaml that delivers ref's templates. A placed Team
// (ref.Folder set) reads its own tenant bundle, per design D13 and the
// tenant-isolation delta: a cell's own bundle never delivers a tenant's
// templates. A synthetic single-controller tenant (ref.Folder empty)
// reads ref's controller's own bundle. It returns a nil slice, with no
// error, when the bundle or the key cannot be found: a job with no
// catalog template still serves record-derived data alone.
func (s *Server) controllerPipelineTemplates(ctx context.Context, ref TenantRef) ([]pipelinetemplate.Document, error) {
	var cm map[string]string
	var err error
	if ref.Folder != "" {
		cm, err = s.teamPipelineBundleContent(ctx, ref)
	} else {
		_, cm, err = s.controllerBundleContent(ctx, ref)
	}
	if err != nil || cm == nil {
		return nil, err
	}
	content, ok := cm["pipeline-templates.yaml"]
	if !ok || content == "" {
		return nil, nil
	}
	var wrapper struct {
		Templates []pipelinetemplate.Document `yaml:"templates"`
	}
	if err := yaml.Unmarshal([]byte(content), &wrapper); err != nil {
		// A malformed pipeline-templates.yaml is already reported by the
		// bundle-validation gate at composition time.
		return nil, nil //nolint:nilerr // best-effort enrichment, see comment above
	}
	return wrapper.Templates, nil
}

// markerContainerPath returns the declared path of a multibranch item
// whose project factory is the marker factory, and whose branches include
// jobPath, together with the fixed marker path from design D6. It reports
// false when jobPath names no such branch: the marker chooses the
// template per branch, so no items-engine entry names a branch job
// directly.
func (s *Server) markerContainerPath(ctx context.Context, ref TenantRef, jobPath string) (string, bool) {
	_, manifest, err := s.controllerItemManifest(ctx, ref)
	if err != nil || manifest == nil {
		return "", false
	}
	for _, ip := range manifest.Flatten() {
		if ip.Item.Kind != "multibranch" {
			continue
		}
		if ip.Item.ProjectFactory == nil || ip.Item.ProjectFactory.VarroaMarker == nil {
			continue
		}
		if strings.HasPrefix(jobPath, ip.Path+"/") {
			return pipelinetemplate.MarkerPath, true
		}
	}
	return "", false
}

// findDeclaredItem returns the item declaring jobPath, from ref's
// controller's composed bundle, and reports whether one was found.
func (s *Server) findDeclaredItem(ctx context.Context, ref TenantRef, jobPath string) (*v1alpha1.Controller, *items.Item, error) {
	cr, manifest, err := s.controllerItemManifest(ctx, ref)
	if err != nil || manifest == nil {
		return cr, nil, err
	}
	for _, ip := range manifest.Flatten() {
		if ip.Path == jobPath {
			item := ip.Item
			return cr, &item, nil
		}
	}
	return cr, nil, nil
}

// declaredJob is one job path a composed bundle declares, with the item
// kind the manifest gives it. Only "pipeline" and "multibranch" reach a
// caller. Every other declared kind is dropped before this value is built.
type declaredJob struct {
	Path string
	Kind string
}

// catalogDeclaredJobPaths lists every pipeline and multibranch item path
// ref's controller's composed bundle declares. Design D8: the job list is
// the union of the record store's job summaries and the jobs the catalog
// declares, because a job that has never run holds no record-store row.
func (s *Server) catalogDeclaredJobPaths(ctx context.Context, ref TenantRef) []declaredJob {
	_, manifest, err := s.controllerItemManifest(ctx, ref)
	if err != nil || manifest == nil {
		return nil
	}
	var jobs []declaredJob
	for _, ip := range manifest.Flatten() {
		if ip.Item.Kind == "pipeline" || ip.Item.Kind == "multibranch" {
			jobs = append(jobs, declaredJob{Path: ip.Path, Kind: ip.Item.Kind})
		}
	}
	return jobs
}

// runTemplatePayload reads the four template-resolution keys design D14
// adds to a run's payload column. A run with no resolved template carries
// none of them.
type runTemplatePayload struct {
	TemplateName    string `json:"template_name"`
	TemplateVersion string `json:"template_version"`
	MarkerPath      string `json:"marker_path"`
	MarkerRevision  string `json:"marker_revision"`
}

// buildPipelineTemplateInfo builds design D12's Template row. An
// items-declared job's data comes from the items engine and the resolved
// catalog template. A marker-defined job's data comes from its most
// recent run when one resolved a template, or from the declared marker
// factory before its first run. It returns nil for a job with no catalog
// template at all.
func (s *Server) buildPipelineTemplateInfo(ctx context.Context, ref TenantRef, jobPath string, item *items.Item, lastRun *recordstore.Run) *pipelineTemplateInfoJSON {
	if item != nil && item.Definition != nil && item.Definition.VarroaTemplate != nil {
		vt := item.Definition.VarroaTemplate
		info := &pipelineTemplateInfoJSON{Name: vt.Template, Version: vt.TemplateVersion, Source: "items"}
		delivered, err := s.controllerPipelineTemplates(ctx, ref)
		if err == nil {
			if doc, rerr := pipelinetemplate.ResolveTemplate(delivered, vt.Template, vt.TemplateVersion); rerr == nil {
				info.Version = doc.Spec.Version
			}
		}
		return info
	}

	var payload runTemplatePayload
	if lastRun != nil && len(lastRun.Payload) > 0 {
		_ = json.Unmarshal(lastRun.Payload, &payload)
	}
	if payload.TemplateName != "" || payload.MarkerPath != "" {
		info := &pipelineTemplateInfoJSON{
			Name:           payload.TemplateName,
			Version:        payload.TemplateVersion,
			MarkerPath:     payload.MarkerPath,
			MarkerRevision: payload.MarkerRevision,
			Source:         "marker",
		}
		if payload.TemplateName != "" && lastRun.BuildNumber != nil {
			info.AsOfBuild = *lastRun.BuildNumber
		}
		return info
	}

	if markerPath, ok := s.markerContainerPath(ctx, ref, jobPath); ok {
		return &pipelineTemplateInfoJSON{MarkerPath: markerPath, Source: "marker"}
	}
	return nil
}

// buildPipelineDefinition converts one declared item into the definition
// row design D8's Defined by card shows. It returns nil when the item
// declares no SCM source, so the Jenkinsfile row stays absent rather than
// showing an empty repository.
func buildPipelineDefinition(item *items.Item) *pipelineDefinitionJSON {
	if item == nil {
		return nil
	}
	repo := ""
	if item.Definition != nil && item.Definition.CpsScmFlowDefinition != nil {
		repo = gitRemoteURL(item.Definition.CpsScmFlowDefinition.SCM.GitSCM)
	}
	if repo == "" && item.Kind == "multibranch" {
		repo = multibranchRemoteURL(item.SourcesList)
	}
	if repo == "" {
		return nil
	}
	return &pipelineDefinitionJSON{JenkinsfileRepo: repo}
}

// gitRemoteURL returns the first configured remote URL of a GitSCM
// definition, or the empty string when none is configured.
func gitRemoteURL(scm *items.GitSCM) string {
	if scm == nil || len(scm.UserRemoteConfigs) == 0 {
		return ""
	}
	return scm.UserRemoteConfigs[0].UserRemoteConfig.URL
}

// multibranchRemoteURL returns the first branch source's remote URL,
// checking both the official-spec branchSource form and the legacy
// top-level git/github form.
func multibranchRemoteURL(sources []items.BranchSourceEntry) string {
	for _, src := range sources {
		if src.Git != nil && src.Git.Remote != "" {
			return src.Git.Remote
		}
		if src.GitHub != nil && src.GitHub.RepositoryURL != "" {
			return src.GitHub.RepositoryURL
		}
		if git := src.BranchSource.Source.Git; git != nil && git.Remote != "" {
			return git.Remote
		}
		if gh := src.BranchSource.Source.GitHub; gh != nil && gh.RepositoryURL != "" {
			return gh.RepositoryURL
		}
	}
	return ""
}

// buildPipelineTriggers converts one declared item's trigger declarations
// into design D8's Triggers card. It returns nil when the item declares
// none, so the card omits the row rather than showing every field false.
func buildPipelineTriggers(item *items.Item) *pipelineTriggersJSON {
	if item == nil {
		return nil
	}
	var all []items.Trigger
	all = append(all, item.Triggers...)
	concurrencyLimited := false
	for _, p := range item.Properties {
		if p.PipelineTriggers != nil {
			all = append(all, p.PipelineTriggers.Triggers...)
		}
		if p.DisableConcurrentBuilds != nil {
			concurrencyLimited = true
		}
	}

	t := &pipelineTriggersJSON{}
	found := false
	for _, trig := range all {
		switch {
		case trig.Cron != nil:
			t.Schedule = trig.Cron.Spec
			found = true
		case trig.PollSCM != nil:
			t.Schedule = trig.PollSCM.ScmpollSpec
			found = true
		}
	}
	if item.Kind == "multibranch" {
		if push, pr := multibranchDiscoveryTraits(item.SourcesList); push || pr {
			t.Push, t.PullRequest = push, pr
			found = true
		}
	}
	if concurrencyLimited {
		t.Concurrency = 1
		found = true
	}
	if !found {
		return nil
	}
	return t
}

// multibranchDiscoveryTraits reports whether any branch source declares
// GitHub branch or pull-request discovery.
func multibranchDiscoveryTraits(sources []items.BranchSourceEntry) (push, pullRequest bool) {
	for _, src := range sources {
		gh := src.GitHub
		if gh == nil {
			gh = src.BranchSource.Source.GitHub
		}
		if gh == nil {
			continue
		}
		for _, trait := range gh.Traits {
			if trait.GitHubBranchDiscovery != nil {
				push = true
			}
			if trait.GitHubPullRequestDiscovery != nil {
				pullRequest = true
			}
		}
	}
	return push, pullRequest
}

// buildPipelineParameters converts one declared item's parameter
// definitions into design D8's JSON-Schema-ready form: string to string,
// boolean to boolean, and choice to string with enum. It reads both the
// item's direct parameters and its parameters property, because a
// pipeline job commonly declares them through the property form.
func buildPipelineParameters(item *items.Item) []pipelineParameterJSON {
	if item == nil {
		return nil
	}
	var all []items.Parameter
	all = append(all, item.Parameters...)
	for _, p := range item.Properties {
		if p.Parameters != nil {
			all = append(all, p.Parameters.ParameterDefinitions...)
		}
	}
	var out []pipelineParameterJSON
	for _, p := range all {
		switch {
		case p.String != nil:
			out = append(out, pipelineParameterJSON{
				Name:         p.String.Name,
				Description:  p.String.Description,
				Kind:         "string",
				Schema:       map[string]any{"type": "string"},
				DefaultValue: p.String.DefaultValue,
			})
		case p.Choice != nil:
			out = append(out, pipelineParameterJSON{
				Name:        p.Choice.Name,
				Description: p.Choice.Description,
				Kind:        "choice",
				Schema:      map[string]any{"type": "string", "enum": p.Choice.Choices},
			})
		case p.BooleanParam != nil:
			out = append(out, pipelineParameterJSON{
				Name:         p.BooleanParam.Name,
				Description:  p.BooleanParam.Description,
				Kind:         "boolean",
				Schema:       map[string]any{"type": "boolean"},
				DefaultValue: strconv.FormatBool(p.BooleanParam.DefaultValue),
			})
		}
	}
	return out
}
