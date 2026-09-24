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
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// declaredByJSON is the Defined by card's provenance data: which input of
// the composed bundle declared this job. Kind names the input's union
// branch. Every key below is complete; no other key exists.
type declaredByJSON struct {
	Kind        string              `json:"kind"`
	Name        string              `json:"name"`
	Revision    string              `json:"revision,omitempty"`
	Bundle      bundleRefJSON       `json:"bundle"`
	RepoURL     string              `json:"repoURL,omitempty"`
	Path        string              `json:"path,omitempty"`
	OCIRef      string              `json:"ociRef,omitempty"`
	CatalogItem *catalogItemRefJSON `json:"catalogItem,omitempty"`
}

// bundleRefJSON names the ComposedBundle CR that owns a declaring input.
type bundleRefJSON struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// catalogItemRefJSON names the CatalogItem an itemRef input references,
// at the namespace it actually resolved in.
type catalogItemRefJSON struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// composedBundleContent reads one ComposedBundle and its content
// ConfigMap by name and namespace. It returns the bundle with a nil map,
// and no error, when the bundle holds no content reference or its
// ConfigMap is absent: a tenant with no catalog template still serves
// record-derived data alone. It returns a nil bundle, with no error, when
// the bundle itself is absent.
func (s *Server) composedBundleContent(ctx context.Context, bundleName, bundleNamespace string) (*v1alpha1.ComposedBundle, map[string]string, error) {
	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, s.deps.Store, bundleName, bundleNamespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if cb.Status.ContentRef == "" {
		return cb, nil, nil
	}

	cm, err := s.deps.Client.GetConfigMap(ctx, cb.Status.ContentRef, bundleNamespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return cb, nil, nil
		}
		return cb, nil, err
	}
	return cb, cm, nil
}

// controllerBundleContent reads ref's controller, its composed bundle, and
// the bundle's content ConfigMap. It returns nil for every value, with no
// error, when ref names no controller or the controller cannot be found. It
// returns the controller with a nil bundle and a nil map when the bundle
// cannot be found, and the controller and the bundle with a nil map when
// the ConfigMap cannot be found. A tenant with no catalog template still
// serves record-derived data alone.
func (s *Server) controllerBundleContent(ctx context.Context, ref TenantRef) (*v1alpha1.Controller, *v1alpha1.ComposedBundle, map[string]string, error) {
	if ref.Controller == "" {
		return nil, nil, nil, nil
	}
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, ref.Controller, ref.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, err
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

	cb, cm, err := s.composedBundleContent(ctx, bundleName, bundleNamespace)
	if err != nil {
		return cr, cb, nil, err
	}
	return cr, cb, cm, nil
}

// teamPipelineBundleContent reads a placed Team's own composed bundle and
// its content ConfigMap, named by Team.spec.pipelines.composedBundleRef.
// It returns a nil bundle and a nil map, with no error, when the Team, its
// pipelines spec, its bundle ref, or the bundle's ConfigMap cannot be
// found: a tenant with no catalog template still serves record-derived
// data alone.
func (s *Server) teamPipelineBundleContent(ctx context.Context, ref TenantRef) (*v1alpha1.ComposedBundle, map[string]string, error) {
	if ref.Team == "" {
		return nil, nil, nil
	}
	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, ref.Team, "")
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if team.Spec.Pipelines == nil || team.Spec.Pipelines.ComposedBundleRef == nil {
		return nil, nil, nil
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
// bundle. It returns the bundle it read, and a nil manifest, with no
// error, when the controller, the bundle, or an items.yaml key cannot be
// found: a tenant with no declared item still serves record-derived data
// alone.
func (s *Server) controllerItemManifest(ctx context.Context, ref TenantRef) (*v1alpha1.Controller, *v1alpha1.ComposedBundle, *items.Manifest, error) {
	var (
		cr  *v1alpha1.Controller
		cb  *v1alpha1.ComposedBundle
		cm  map[string]string
		err error
	)
	if ref.Folder != "" {
		// A placed Team never reads the cell's bundle; the controller is
		// fetched only for its version.
		cr, err = s.tenantController(ctx, ref)
		if err != nil {
			return nil, nil, nil, err
		}
		cb, cm, err = s.teamPipelineBundleContent(ctx, ref)
	} else {
		cr, cb, cm, err = s.controllerBundleContent(ctx, ref)
	}
	if err != nil {
		return cr, cb, nil, err
	}
	if cm == nil {
		return cr, cb, nil, nil
	}
	content, ok := cm["items.yaml"]
	if !ok || content == "" {
		return cr, cb, nil, nil
	}
	manifest, err := items.Parse(content)
	if err != nil {
		// An items.yaml that fails to parse leaves the pipeline surface
		// without a definition rather than failing the read: the
		// bundle-validation gate is the place that rejects a bad manifest.
		return cr, cb, nil, nil //nolint:nilerr // best-effort enrichment, see comment above
	}
	if ref.Folder != "" {
		manifest, err = items.ReRoot(manifest, ref.Folder)
		if err != nil {
			return cr, cb, nil, nil //nolint:nilerr // a tenant manifest that names its own root is rejected upstream
		}
	}
	return cr, cb, manifest, nil
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
		_, cm, err = s.teamPipelineBundleContent(ctx, ref)
	} else {
		_, _, cm, err = s.controllerBundleContent(ctx, ref)
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
	_, _, manifest, err := s.controllerItemManifest(ctx, ref)
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
// controller's composed bundle, and reports whether one was found. It
// also returns the bundle input that declared the item. It resolves that
// input only when the item's definition renders, so an item with no
// definition carries no provenance.
func (s *Server) findDeclaredItem(ctx context.Context, ref TenantRef, jobPath string) (*v1alpha1.Controller, *items.Item, *declaredByJSON, error) {
	cr, cb, manifest, err := s.controllerItemManifest(ctx, ref)
	if err != nil || manifest == nil {
		return cr, nil, nil, err
	}
	for _, ip := range manifest.Flatten() {
		if ip.Path == jobPath {
			item := ip.Item
			var declaredBy *declaredByJSON
			if buildPipelineDefinition(&item) != nil {
				declaredBy = s.resolveDeclaredInput(ctx, cb, ref, jobPath)
			}
			return cr, &item, declaredBy, nil
		}
	}
	return cr, nil, nil, nil
}

// resolveDeclaredInput determines which input of cb declared jobPath.
// Called only after the merged manifest already resolved jobPath to an
// item whose definition renders (buildPipelineDefinition returned
// non-nil), so it performs no manifest walk of its own — it examines
// cb.Spec.Inputs, already in memory on the fetched cb, plus a bounded
// number of CatalogItem reads for itemRef candidates. Returns nil when
// cb is nil (bundle not found) or when no input resolves.
func (s *Server) resolveDeclaredInput(ctx context.Context, cb *v1alpha1.ComposedBundle, ref TenantRef, jobPath string) *declaredByJSON {
	if cb == nil {
		return nil
	}
	bundleRef := bundleRefJSON{Namespace: cb.Namespace, Name: cb.Name}

	for i, input := range cb.Spec.Inputs {
		if input.ItemRef == nil {
			continue
		}
		var summary v1alpha1.InputSummaryEntry
		if i < len(cb.Status.InputSummary) {
			summary = cb.Status.InputSummary[i]
		}
		if summary.Namespace == "" {
			// An entry with no namespace means the input is unresolved,
			// so it cannot be the declarer.
			continue
		}
		catalogItem, err := crdstore.Get[v1alpha1.CatalogItem](ctx, s.deps.Store, input.ItemRef.Name, summary.Namespace)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				continue // deleted since the last reconcile: not this one
			}
			// A transient read error is terminal for the whole
			// resolution: it must never let step 2 name a git/OCI input
			// in this itemRef's place.
			s.deps.Logger.Warn("declared-input itemRef read failed", "jobPath", jobPath, "item", input.ItemRef.Name, "error", err)
			return nil
		}
		if catalogItem.Spec.Type != v1alpha1.CatalogItemItem {
			continue
		}
		manifest, err := items.Parse(catalogItem.Status.Content)
		if err != nil {
			continue // malformed content: not a match, not a failure of this call
		}
		// The merge drops each input's root key. Clear Root here, so the
		// paths compare in the merged path space.
		manifest.Root = ""
		if ref.Folder != "" {
			manifest, err = items.ReRoot(manifest, ref.Folder)
			if err != nil {
				// ReRoot cannot fail once Root is clear. A failure here is
				// unexpected, so answer no declarer.
				s.deps.Logger.Warn("declared-input itemRef re-root failed", "jobPath", jobPath, "item", input.ItemRef.Name, "error", err)
				return nil
			}
		}
		for _, ip := range manifest.Flatten() {
			if ip.Path != jobPath {
				continue
			}
			return &declaredByJSON{
				Kind:     "itemRef",
				Name:     input.ItemRef.Name,
				Revision: revisionForInput(cb, i, input.ItemRef.Name),
				Bundle:   bundleRef,
				CatalogItem: &catalogItemRefJSON{
					Name:      input.ItemRef.Name,
					Namespace: summary.Namespace,
				},
			}
		}
	}

	for i, input := range cb.Spec.Inputs {
		switch {
		case input.GitSource != nil:
			return &declaredByJSON{
				Kind:     "gitSource",
				Name:     input.GitSource.RepoURL,
				RepoURL:  input.GitSource.RepoURL,
				Path:     input.GitSource.Path,
				Revision: revisionForInput(cb, i, ""),
				Bundle:   bundleRef,
			}
		case input.OCISource != nil:
			return &declaredByJSON{
				Kind:     "ociSource",
				Name:     input.OCISource.Ref,
				OCIRef:   input.OCISource.Ref,
				Path:     input.OCISource.Path,
				Revision: revisionForInput(cb, i, ""),
				Bundle:   bundleRef,
			}
		}
	}
	return nil
}

// revisionForInput reads ComposedBundle.status.observedRevisions for
// input index i, falling back to the itemRefName key when itemRefName is
// non-empty and the index key is empty — the same two-key read
// projectResolvedInputs (handlers_clusters.go) performs. Returns "" for
// the operator's ItemMissingRevision sentinel, which is not a revision.
func revisionForInput(cb *v1alpha1.ComposedBundle, i int, itemRefName string) string {
	rev := cb.Status.ObservedRevisions[strconv.Itoa(i)]
	if rev == "" && itemRefName != "" {
		rev = cb.Status.ObservedRevisions[itemRefName]
	}
	if rev == controller.ItemMissingRevision {
		return ""
	}
	return rev
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
	_, _, manifest, err := s.controllerItemManifest(ctx, ref)
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

// runTemplatePayloadOf reads the four template-resolution keys out of a
// run's payload column. The recorder stores each event's whole envelope
// under its kind key, so the keys sit at
// payload -> 'run.started' -> 'payload' and
// payload -> 'run.completed' -> 'payload', never at the top level.
//
// It answers the started copy when that copy names a template or a
// marker, and the completed copy otherwise. The completed event carries
// the same keys, so a run whose start event the store never recorded
// still reads as template-defined, and the replay refusal holds. An
// absent or undecodable path answers the zero value, which names no
// template.
func runTemplatePayloadOf(payload json.RawMessage) runTemplatePayload {
	if len(payload) == 0 {
		return runTemplatePayload{}
	}
	var doc struct {
		Started struct {
			Payload runTemplatePayload `json:"payload"`
		} `json:"run.started"`
		Completed struct {
			Payload runTemplatePayload `json:"payload"`
		} `json:"run.completed"`
	}
	if json.Unmarshal(payload, &doc) != nil {
		return runTemplatePayload{}
	}
	if started := doc.Started.Payload; started.TemplateName != "" || started.MarkerPath != "" {
		return started
	}
	return doc.Completed.Payload
}

// runSourcePayload reads the script path a run's source column records.
// The stored envelope writes snake_case keys, so this type tags the key
// the envelope carries, not a response DTO's camelCase.
type runSourcePayload struct {
	DefinitionPath string `json:"definition_path,omitempty"`
}

// lastRunDefinitionPath returns the Jenkinsfile's path within its own
// repository, as the job's last run recorded it. It returns "" when the
// run is absent, carries no source, or carries malformed JSON.
func lastRunDefinitionPath(lastRun *recordstore.Run) string {
	if lastRun == nil || len(lastRun.Source) == 0 {
		return ""
	}
	var src runSourcePayload
	if err := json.Unmarshal(lastRun.Source, &src); err != nil {
		return ""
	}
	return src.DefinitionPath
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
	if lastRun != nil {
		payload = runTemplatePayloadOf(lastRun.Payload)
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
// declares no SCM source, no inline script, and no template, so the row stays absent rather than
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
	if repo != "" {
		return &pipelineDefinitionJSON{Source: "jenkinsfile", JenkinsfileRepo: repo}
	}
	// An item that carries its script inline has no repository to name.
	// The card still states where the script lives, so the row never
	// reads as missing data.
	if item.Definition != nil && item.Definition.Script != "" {
		return &pipelineDefinitionJSON{Source: "inline"}
	}
	// A template renders the script; the template row names it, and the
	// definition still carries the profile the job runs on.
	if item.Definition != nil && item.Definition.VarroaTemplate != nil {
		return &pipelineDefinitionJSON{Source: "template"}
	}
	return nil
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
