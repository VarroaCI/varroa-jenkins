package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
)

// ItemLookup fetches CatalogItem CRDs for the composer.
type ItemLookup interface {
	GetCatalogItemCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogItem, error)
}

// Composer composes inputs (catalog items and git sources) into a
// MaterializedBundle. Git inputs are materialized via the Resolver;
// catalog items are read from the cluster.
type Composer struct {
	lookup            ItemLookup
	resolver          *Resolver
	workDir           string
	operatorNamespace string
}

// NewComposer creates a new Composer.
func NewComposer(lookup ItemLookup, resolver *Resolver, workDir, operatorNamespace string) *Composer {
	return &Composer{
		lookup:            lookup,
		resolver:          resolver,
		workDir:           workDir,
		operatorNamespace: operatorNamespace,
	}
}

// ComposeResult holds the output of a composition.
type ComposeResult struct {
	// Materialized is the unresolved merged bundle content.
	Materialized *MaterializedBundle
	// ResolvedHash is sha256 over the unresolved content + sorted raw vars.
	ResolvedHash string
	// BundleYAML is the concatenated YAML for display.
	BundleYAML string
	// Missing lists referenced items or git inputs that could not be resolved.
	Missing []string
	// Drifted lists item names whose content hash differs from the pinned value.
	Drifted []string
	// Warnings holds non-fatal issues (e.g. plugin version conflicts).
	Warnings []string
	// Errors holds validation errors discovered during composition.
	Errors []string
}

// Compose composes a ComposedBundleSpec into a MaterializedBundle.
// It handles both catalog item refs and git source inputs. The returned
// content is unresolved — ${var} placeholders are retained for per-controller
// resolution later.
//
// resolvedAuth may be nil. When set, it provides pre-resolved GitAuth for
// git inputs (keyed by input index). The caller is responsible for reading
// Secrets and converting them to GitAuth.
//
// resolvedOCIAuth may be nil. When set, it provides pre-resolved OCIAuth for
// OCI inputs (keyed by input index). The caller is responsible for reading
// Secrets and converting them to OCIAuth.
func (c *Composer) Compose(ctx context.Context, ns string, spec *v1alpha1.ComposedBundleSpec, resolvedAuth map[int]*GitAuth, resolvedOCIAuth map[int]*OCIAuth) (*ComposeResult, error) {
	result := &ComposeResult{}

	// Validate union inputs — exactly one of itemRef, gitSource, or ociSource must be set.
	for i, input := range spec.Inputs {
		switch {
		case input.ItemRef == nil && input.GitSource == nil && input.OCISource == nil:
			return nil, fmt.Errorf("input[%d]: must set exactly one of itemRef, gitSource, or ociSource, none set", i)
		case input.ItemRef != nil && input.GitSource == nil && input.OCISource == nil:
			// valid — single field
		case input.ItemRef == nil && input.GitSource != nil && input.OCISource == nil:
			// valid — single field
		case input.ItemRef == nil && input.GitSource == nil && input.OCISource != nil:
			// valid — single field
		default:
			return nil, fmt.Errorf("input[%d]: must set exactly one of itemRef, gitSource, or ociSource, multiple set", i)
		}
	}

	// Step 1: Collect content from each input in list order.
	// Each input contributes its type-grouped content for merging.
	groups := map[string][]string{
		"jcasc":       {},
		"podtemplate": {},
		"plugin":      {},
		"item":        {},
		"rbac":        {},
	}

	// itemSources names the source of each entry in groups["item"], in the
	// same order. composeItems uses it to name a dropped top-level key.
	var itemSources []string

	// Raw variables from all sources. Precedence (lowest to highest):
	// 1. Item defaults — fill unset keys only
	// 2. Spec-wide variables — override defaults
	// 3. Per-ref variables — override everything (highest user precedence)
	rawVars := make(Variables)

	// pipelineTemplates collects the templates the bundle's pipeline-template
	// inputs carry, in input order (design D5, D7). ptSeen de-duplicates by
	// name+version: identical content is a harmless duplicate, differing
	// content is an identity collision naming both itemRefs.
	var pipelineTemplates []pipelinetemplate.Document
	ptSeen := make(map[string]struct {
		refName string
		content string
	})

	// Apply spec-wide variables first so per-ref can override them.
	for k, v := range spec.Variables {
		rawVars[k] = v
	}

	for i, input := range spec.Inputs {
		switch {
		case input.ItemRef != nil:
			ref := input.ItemRef
			var item *v1alpha1.CatalogItem
			var err error
			if ref.Namespace != "" {
				// Explicit namespace: exact lookup only, no fallback (rows 1-2).
				// NEVER emits a shadowing warning — naming a namespace IS the explicit choice.
				item, err = c.lookup.GetCatalogItemCRD(ctx, ref.Name, ref.Namespace)
				if err != nil || item == nil {
					result.Missing = append(result.Missing, ref.Namespace+"/"+ref.Name)
					continue
				}
			} else {
				// Unset: E's S1 chain, unchanged (rows 3-6).
				item, err = c.lookup.GetCatalogItemCRD(ctx, ref.Name, ns)
				localHit := err == nil && item != nil // capture BEFORE the fallback attempt
				if !localHit && c.operatorNamespace != "" && c.operatorNamespace != ns {
					item, err = c.lookup.GetCatalogItemCRD(ctx, ref.Name, c.operatorNamespace)
				}
				if err != nil || item == nil {
					result.Missing = append(result.Missing, ref.Name)
					continue
				}
				// Row 4: shadow check fires ONLY on a local hit (never on a fallback hit —
				// a fallback hit means the operator-ns item itself was used, nothing is shadowed).
				if localHit && c.operatorNamespace != "" && c.operatorNamespace != ns {
					if shadow, sErr := c.lookup.GetCatalogItemCRD(ctx, ref.Name, c.operatorNamespace); sErr == nil && shadow != nil {
						result.Warnings = append(result.Warnings, fmt.Sprintf(
							"itemRef %q: using %s/%s; a same-named item exists in the operator namespace (%s/%s) and is shadowed — set itemRef.namespace to choose explicitly",
							ref.Name, ns, ref.Name, c.operatorNamespace, ref.Name))
					}
				}
			}

			// Check for drift against pinned content hash.
			if ref.PinnedContentHash != "" && item.Status.ContentHash != ref.PinnedContentHash {
				result.Drifted = append(result.Drifted, ref.Name)
			}

			// An invalid item must fail the compose loudly, not be silently
			// omitted: its content is empty by contract (invalid items store
			// nothing), and the catalog sync already recorded why in
			// status.message.
			if !item.Status.Valid {
				result.Errors = append(result.Errors, fmt.Sprintf(
					"itemRef %q is invalid and cannot be composed: %s", ref.Name, item.Status.Message))
				continue
			}

			groupKey := string(item.Spec.Type)
			if groupKey == "groovy" {
				result.Errors = append(result.Errors, fmt.Sprintf(
					"itemRef %q: catalog item type %q is a brood-operation-only type and cannot be used as a ComposedBundle input",
					ref.Name, item.Spec.Type))
				continue
			}
			if groupKey == "pipeline-template" {
				// pipeline-template content is a PipelineTemplate document
				// (design D2). It composes into its own template set, never
				// into ItemsYAML, and never resolves a ${var} placeholder —
				// so it skips the CatalogVariable merging below.
				doc, err := pipelinetemplate.ParseDocument([]byte(item.Status.Content))
				if err != nil {
					result.Errors = append(result.Errors, fmt.Sprintf(
						"itemRef %q: pipeline-template content failed to parse: %v", ref.Name, err))
					continue
				}
				key := doc.Metadata.Name + "@" + doc.Spec.Version
				if prior, exists := ptSeen[key]; exists {
					if prior.content != item.Status.Content {
						result.Errors = append(result.Errors, fmt.Sprintf(
							"pipeline-template %q and %q both declare name %q version %q with different content",
							prior.refName, ref.Name, doc.Metadata.Name, doc.Spec.Version))
					}
					continue
				}
				ptSeen[key] = struct {
					refName string
					content string
				}{refName: ref.Name, content: item.Status.Content}
				pipelineTemplates = append(pipelineTemplates, *doc)
				continue
			}
			if _, ok := groups[groupKey]; ok {
				content := item.Status.Content
				// A jcasc item may embed a top-level `plugins:` block so it can
				// ship the plugin its config depends on (e.g. varroa-theme
				// bundling simple-theme-plugin). Split it out to the
				// plugin set; the remaining jcasc config routes as usual.
				if groupKey == "jcasc" {
					jcascOnly, pluginsOnly := splitEmbeddedPlugins(content)
					if strings.TrimSpace(pluginsOnly) != "" {
						groups["plugin"] = append(groups["plugin"], pluginsOnly)
					}
					content = jcascOnly
				}
				if strings.TrimSpace(content) != "" {
					groups[groupKey] = append(groups[groupKey], content)
					if groupKey == "item" {
						refName := ref.Name
						if ref.Namespace != "" {
							refName = ref.Namespace + "/" + ref.Name
						}
						itemSources = append(itemSources, fmt.Sprintf("itemRef %q", refName))
					}
				}
			}

			// Collect item default variables (lowest precedence — only fill unset).
			for _, v := range item.Spec.Variables {
				if v.Default != "" {
					if _, exists := rawVars[v.Name]; !exists {
						rawVars[v.Name] = v.Default
					}
				}
			}

			// Per-ref variables.
			for k, v := range ref.Variables {
				rawVars[k] = v
			}

		case input.GitSource != nil:
			gs := input.GitSource
			if c.resolver == nil {
				return nil, fmt.Errorf("input[%d]: git source requires a resolver", i)
			}

			// Use pre-resolved auth from caller, or nil for public repos.
			var auth *GitAuth
			if resolvedAuth != nil {
				auth = resolvedAuth[i]
			}
			if gs.SecretRef != "" && auth == nil {
				return nil, fmt.Errorf("input[%d]: git auth via secretRef requires caller to resolve secret first", i)
			}

			// Ensure the work dir exists before MkdirTemp — on ephemeral
			// /tmp it can be absent after an operator restart, which would
			// otherwise hard-fail every git-input compose.
			if err := os.MkdirAll(c.workDir, 0o755); err != nil {
				return nil, fmt.Errorf("input[%d]: ensure compose work dir: %w", i, err)
			}
			cloneDir, err := os.MkdirTemp(c.workDir, "composed-input-*")
			if err != nil {
				return nil, fmt.Errorf("input[%d]: create temp clone dir: %w", i, err)
			}
			defer func() { _ = os.RemoveAll(cloneDir) }() // clean up after materialization

			mat, err := c.resolver.Materialize(ctx, gs.RepoURL, gs.Path, gs.Revision, cloneDir, auth)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("git input[%d] (%s): %v", i, gs.RepoURL, err))
				continue
			}

			// Merge git content into groups.
			if mat.JenkinsYAML != "" {
				groups["jcasc"] = append(groups["jcasc"], mat.JenkinsYAML)
			}
			if mat.PluginsYAML != "" {
				groups["plugin"] = append(groups["plugin"], mat.PluginsYAML)
			}
			if mat.ItemsYAML != "" {
				groups["item"] = append(groups["item"], mat.ItemsYAML)
				itemSources = append(itemSources, fmt.Sprintf("git input[%d] (%s)", i, gs.RepoURL))
			}
			if mat.RbacYAML != "" {
				groups["rbac"] = append(groups["rbac"], mat.RbacYAML)
			}

			// Merge git-loaded variables.
			for k, v := range mat.Variables {
				rawVars[k] = v
			}

		case input.OCISource != nil:
			ocs := input.OCISource
			if c.resolver == nil {
				return nil, fmt.Errorf("input[%d]: OCI source requires a resolver", i)
			}

			// Use pre-resolved OCI auth from caller, or nil for public artifacts.
			var auth *OCIAuth
			if resolvedOCIAuth != nil {
				auth = resolvedOCIAuth[i]
			}
			if ocs.SecretRef != "" && auth == nil {
				return nil, fmt.Errorf("input[%d]: OCI auth via secretRef requires caller to resolve secret first", i)
			}

			// Ensure the work dir exists before MkdirTemp.
			if err := os.MkdirAll(c.workDir, 0o755); err != nil {
				return nil, fmt.Errorf("input[%d]: ensure compose work dir: %w", i, err)
			}
			cloneDir, err := os.MkdirTemp(c.workDir, "composed-oci-input-*")
			if err != nil {
				return nil, fmt.Errorf("input[%d]: create temp OCI clone dir: %w", i, err)
			}
			defer func() { _ = os.RemoveAll(cloneDir) }()

			mat, err := c.resolver.MaterializeOCI(ctx, ocs.Ref, ocs.Path, cloneDir, auth)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("OCI input[%d] (%s): %v", i, ocs.Ref, err))
				continue
			}

			// Merge OCI content into groups (identical to GitSource merge).
			if mat.JenkinsYAML != "" {
				groups["jcasc"] = append(groups["jcasc"], mat.JenkinsYAML)
			}
			if mat.PluginsYAML != "" {
				groups["plugin"] = append(groups["plugin"], mat.PluginsYAML)
			}
			if mat.ItemsYAML != "" {
				groups["item"] = append(groups["item"], mat.ItemsYAML)
				itemSources = append(itemSources, fmt.Sprintf("OCI input[%d] (%s)", i, ocs.Ref))
			}
			if mat.RbacYAML != "" {
				groups["rbac"] = append(groups["rbac"], mat.RbacYAML)
			}

			// Merge OCI-loaded variables.
			for k, v := range mat.Variables {
				rawVars[k] = v
			}
		}
	}

	// Step 2: Merge each section.

	// Determine jcasc merge strategy.
	strategy := spec.JcascMergeStrategy
	if strategy == "" {
		strategy = "errorOnConflict"
	}

	// Merge jcasc items first, then inject pod templates into the merged
	// result. Pod templates can't be merged as a separate jcasc document
	// because jenkins.clouds is a list — mergeMaps replaces lists wholesale
	// on conflict rather than deep-merging them, which would clobber a
	// jcasc item's full cloud config (name/namespace/jenkinsUrl) with the
	// pod-template wrapper's bare templates-only entry.
	var jenkinsYAML string
	if len(groups["jcasc"]) > 0 || len(groups["podtemplate"]) > 0 {
		merged, err := inlineMergeJcasc(groups["jcasc"], strategy)
		if err != nil {
			return nil, fmt.Errorf("merge jcasc: %w", err)
		}
		if len(groups["podtemplate"]) > 0 {
			merged, err = injectPodTemplatesIntoJCasC(merged, groups["podtemplate"])
			if err != nil {
				return nil, fmt.Errorf("inject pod templates: %w", err)
			}
		}
		jenkinsYAML = merged
	}

	pluginsYAML, pluginWarnings := composePlugins(groups["plugin"])
	result.Warnings = append(result.Warnings, pluginWarnings...)

	itemsYAML, droppedItemKeys, err := composeItems(groups["item"], itemSources)
	if err != nil {
		return nil, fmt.Errorf("compose items: %w", err)
	}
	for _, d := range droppedItemKeys {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"item manifest from %s: top-level key %q is dropped; a composed bundle keeps only \"items\"", d.Source, d.Key))
	}
	rbacYAML, err := composeRbac(groups["rbac"])
	if err != nil {
		return nil, fmt.Errorf("compose rbac: %w", err)
	}
	pipelineTemplatesYAML, err := composePipelineTemplates(pipelineTemplates)
	if err != nil {
		return nil, fmt.Errorf("compose pipeline templates: %w", err)
	}

	// An items-declared definition.varroaTemplate is fully visible at
	// composition time (design D10), unlike a marker: validate its values
	// against the resolved template's schema now, rather than waiting for
	// L3 at run start.
	result.Errors = append(result.Errors, validateItemTemplateValues(itemsYAML, pipelineTemplates)...)

	// NOTE: varroa_* auto-vars are NOT injected here. They are injected at
	// controller resolve time via ResolveVars.

	// Step 4: Run validation floor on merged content.
	// Errors are recorded in result.Errors and surfaced via ComposedBundle
	// status. The caller decides whether to treat validation failures as fatal.
	validation := ValidateContent(jenkinsYAML, pluginsYAML, itemsYAML, rbacYAML, rawVars)
	result.Errors = append(result.Errors, validation.Errors...)
	result.Warnings = append(result.Warnings, validation.Warnings...)

	// Step 5: Build MaterializedBundle (unresolved).
	result.Materialized = &MaterializedBundle{
		JenkinsYAML:           jenkinsYAML,
		PluginsYAML:           pluginsYAML,
		ItemsYAML:             itemsYAML,
		RbacYAML:              rbacYAML,
		Variables:             rawVars,
		PipelineTemplatesYAML: pipelineTemplatesYAML,
		DroppedItemKeys:       droppedItemKeys,
	}

	// Step 6: Compute hash over unresolved content + raw vars.
	result.ResolvedHash = computeResolvedHash(jenkinsYAML, pluginsYAML, itemsYAML, rbacYAML, pipelineTemplatesYAML, rawVars)

	// Step 6: BundleYAML preview — concatenate all sections with "---\n" separators.
	var bundleParts []string
	for _, s := range []string{jenkinsYAML, pluginsYAML, itemsYAML, rbacYAML} {
		if s != "" {
			bundleParts = append(bundleParts, s)
		}
	}
	result.BundleYAML = strings.Join(bundleParts, "\n---\n")

	return result, nil
}

// inlineMergeJcasc merges multiple JCasC YAML strings using mergeMaps.
func inlineMergeJcasc(yamls []string, strategy string) (string, error) {
	var merged map[string]any
	for _, y := range yamls {
		if strings.TrimSpace(y) == "" {
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
			return "", fmt.Errorf("unmarshal jcasc yaml: %w", err)
		}
		if merged == nil {
			merged = doc
		} else {
			if err := mergeMaps(merged, doc, strategy); err != nil {
				return "", fmt.Errorf("merge: %w", err)
			}
		}
	}

	if merged == nil {
		return "", nil
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("marshal merged jcasc: %w", err)
	}
	return string(out), nil
}

// MergeJenkinsYAML merges a version-profile JCasC overlay on top of a base
// jenkins.yaml. The overlay wins on scalar key conflicts (maps are deep-merged).
// Empty base or overlay short-circuits to the other.
func MergeJenkinsYAML(base, overlay string) (string, error) {
	if strings.TrimSpace(overlay) == "" {
		return base, nil
	}
	if strings.TrimSpace(base) == "" {
		return overlay, nil
	}
	return inlineMergeJcasc([]string{base, overlay}, "override")
}

// injectPodTemplatesIntoJCasC merges podtemplate catalog item contents into
// an already-merged jcasc YAML document, appending each pod-template entry
// to jenkins.clouds[].kubernetes.templates. Injecting into the parsed
// document (rather than merging a separate jenkins.clouds document via
// mergeMaps) is required because jenkins.clouds is a list — mergeMaps
// replaces lists wholesale on conflict instead of deep-merging them, which
// would clobber a jcasc item's full cloud config (name/namespace/
// jenkinsUrl) with a bare templates-only entry.
//
// Templates are injected into every kubernetes cloud entry found, since
// podtemplate items carry no target-cloud reference. If no kubernetes cloud
// exists yet, a minimal one is created. Returns jcascYAML unchanged when
// there are no pod templates to inject.
func injectPodTemplatesIntoJCasC(jcascYAML string, contents []string) (string, error) {
	var templates []any
	for _, c := range contents {
		if strings.TrimSpace(c) == "" {
			continue
		}
		var entries []any
		if err := yaml.Unmarshal([]byte(c), &entries); err != nil {
			// Skip unparseable content; validation surfaces this at sync time.
			continue
		}
		templates = append(templates, entries...)
	}

	if len(templates) == 0 {
		return jcascYAML, nil
	}

	var doc map[string]any
	if strings.TrimSpace(jcascYAML) != "" {
		if err := yaml.Unmarshal([]byte(jcascYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for pod template injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	var jenkins map[string]any
	if raw, exists := doc["jenkins"]; exists {
		m, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins\" key is not a map (got %T)", raw)
		}
		jenkins = m
	}
	if jenkins == nil {
		jenkins = map[string]any{}
		doc["jenkins"] = jenkins
	}

	var clouds []any
	if raw, exists := jenkins["clouds"]; exists {
		c, ok := raw.([]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins.clouds\" is not a list (got %T)", raw)
		}
		clouds = c
	}

	injected := false
	for _, cloud := range clouds {
		cloudMap, ok := cloud.(map[string]any)
		if !ok {
			continue
		}
		raw, hasK8s := cloudMap["kubernetes"]
		if !hasK8s {
			continue
		}
		k8s, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("jcasc: a \"kubernetes\" cloud entry is not a map (got %T)", raw)
		}
		existing, _ := k8s["templates"].([]any)
		k8s["templates"] = append(existing, templates...)
		injected = true
	}

	if !injected {
		jenkins["clouds"] = append(clouds, map[string]any{
			"kubernetes": map[string]any{"templates": templates},
		})
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after pod template injection: %w", err)
	}
	return string(out), nil
}

// InjectRunnerNodes appends runner nodes to jenkins.nodes in an
// already-merged jcasc document (design D3). A map merge cannot be used:
// mergeMaps replaces a list on conflict, so it would delete every node the
// cell bundle declares. Exported because the caller is package controller,
// not package bundle: runner nodes are per-cell data the Controller
// reconciler assembles per desired-state push, not content the composer
// bakes into the shared ComposedBundle ConfigMap.
//
// It always writes jenkins.nodes, as an empty list when the document
// declares none and nodes is empty: JCasC replaces the node list it is
// given, so an explicit empty list is what drops the runner nodes of a
// tenant that left the cell, and an absent key would leave them behind.
// It rejects a node whose name collides with one the document already
// declares.
func InjectRunnerNodes(jcascYAML string, nodes []any) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(jcascYAML) != "" {
		if err := yaml.Unmarshal([]byte(jcascYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for runner node injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	var jenkins map[string]any
	if raw, exists := doc["jenkins"]; exists {
		m, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins\" key is not a map (got %T)", raw)
		}
		jenkins = m
	}
	if jenkins == nil {
		jenkins = map[string]any{}
		doc["jenkins"] = jenkins
	}

	var existingNodes []any
	if raw, exists := jenkins["nodes"]; exists {
		n, ok := raw.([]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins.nodes\" is not a list (got %T)", raw)
		}
		existingNodes = n
	}

	existingNames := make(map[string]bool, len(existingNodes))
	for _, n := range existingNodes {
		if name, ok := permanentNodeName(n); ok {
			existingNames[name] = true
		}
	}

	for _, n := range nodes {
		name, ok := permanentNodeName(n)
		if !ok {
			continue
		}
		if existingNames[name] {
			return "", fmt.Errorf("jcasc: node %q collides with a node the bundle already declares", name)
		}
		existingNames[name] = true
	}

	merged := make([]any, 0, len(existingNodes)+len(nodes))
	merged = append(merged, existingNodes...)
	merged = append(merged, nodes...)
	jenkins["nodes"] = merged

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after runner node injection: %w", err)
	}
	return string(out), nil
}

// permanentNodeName reads the name field off a jenkins.nodes entry shaped
// {permanent: {name: ...}}, the only node shape this package renders or
// expects to find already declared by a bundle.
func permanentNodeName(entry any) (string, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return "", false
	}
	permanent, ok := m["permanent"].(map[string]any)
	if !ok {
		return "", false
	}
	name, ok := permanent["name"].(string)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// splitEmbeddedPlugins separates a top-level `plugins:` block out of a jcasc
// item's content so it can be routed to the plugin set rather than leaking
// into jenkins.yaml. This lets a single jcasc catalog item ship the plugin its
// configuration depends on (e.g. the varroa-theme item bundling
// simple-theme-plugin). It returns the content with the plugins key
// removed and a standalone plugins document. When the content has no plugins
// key (the common case for every other jcasc item) it is returned verbatim so
// existing behavior is unchanged.
func splitEmbeddedPlugins(content string) (jcascYAML, pluginsYAML string) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil || doc == nil {
		return content, ""
	}
	raw, ok := doc["plugins"]
	if !ok {
		return content, ""
	}
	delete(doc, "plugins")
	pluginsDoc, err := yaml.Marshal(map[string]any{"plugins": raw})
	if err != nil {
		// Never expected (raw came from a successful unmarshal), but on any
		// failure return the content verbatim rather than silently dropping
		// the embedded plugin, which would crashloop the controller.
		return content, ""
	}
	if len(doc) == 0 {
		return "", string(pluginsDoc)
	}
	rest, err := yaml.Marshal(doc)
	if err != nil {
		return content, ""
	}
	return string(rest), string(pluginsDoc)
}

// composePlugins deduplicates plugin entries by artifactId. Later entries win.
// Returns the merged plugins YAML and any version conflict warnings.
func composePlugins(yamls []string) (string, []string) {
	type pluginEntry struct {
		ArtifactId string `yaml:"artifactId"`
		Version    string `yaml:"version"`
	}

	seen := make(map[string]string) // artifactId -> version
	order := make([]string, 0)      // insertion order
	var warnings []string

	for _, y := range yamls {
		if strings.TrimSpace(y) == "" {
			continue
		}
		var wrapper struct {
			Plugins []pluginEntry `yaml:"plugins"`
		}
		if err := yaml.Unmarshal([]byte(y), &wrapper); err != nil {
			continue
		}
		for _, p := range wrapper.Plugins {
			if existing, exists := seen[p.ArtifactId]; exists {
				if existing != p.Version {
					warnings = append(warnings, fmt.Sprintf("plugin %s: version conflict (%s vs %s), using %s", p.ArtifactId, existing, p.Version, p.Version))
				}
			} else {
				order = append(order, p.ArtifactId)
			}
			seen[p.ArtifactId] = p.Version
		}
	}

	if len(order) == 0 {
		return "", warnings
	}

	// Output in first-seen order.
	plugins := make([]pluginEntry, 0, len(order))
	for _, id := range order {
		plugins = append(plugins, pluginEntry{ArtifactId: id, Version: seen[id]})
	}

	out, _ := yaml.Marshal(map[string]any{"plugins": plugins})
	return string(out), warnings
}

// composeItems concatenates item YAML content under a single items key. It
// also returns each top-level key other than items that a fragment declares
// and the composer drops. sources[i] names the source of yamls[i].
func composeItems(yamls []string, sources []string) (string, []DroppedItemKey, error) {
	var items []map[string]any
	var dropped []DroppedItemKey
	for i, y := range yamls {
		if strings.TrimSpace(y) == "" {
			continue
		}
		source := sources[i]
		dropped = append(dropped, droppedTopLevelKeys(y, source)...)
		var wrapper struct {
			Items []map[string]any `yaml:"items"`
		}
		if err := yaml.Unmarshal([]byte(y), &wrapper); err != nil {
			return "", nil, fmt.Errorf("%s: %w", source, err)
		}
		items = append(items, wrapper.Items...)
	}

	if len(items) == 0 {
		return "", dropped, nil
	}
	out, _ := yaml.Marshal(map[string]any{"items": items})
	return string(out), dropped, nil
}

// droppedTopLevelKeys lists the top-level mapping keys of one item fragment
// other than items and a merge key, in document order. A fragment that does
// not parse as a mapping gives nothing; the wrapper unmarshal reports it.
func droppedTopLevelKeys(y, source string) []DroppedItemKey {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var out []DroppedItemKey
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if key == "items" || isMergeKeyNode(root.Content[i]) {
			continue
		}
		out = append(out, DroppedItemKey{Source: source, Key: key})
	}
	return out
}

// isMergeKeyNode reports whether k is a YAML merge key, by the same test as
// yaml.v3. A quoted "<<" is an ordinary key.
func isMergeKeyNode(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" &&
		(k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

// composeRbac merges RBAC YAML content under a single roles key.
func composeRbac(yamls []string) (string, error) {
	roles := make(map[string]any)
	for i, y := range yamls {
		if strings.TrimSpace(y) == "" {
			continue
		}
		var wrapper struct {
			Roles map[string]any `yaml:"roles"`
		}
		if err := yaml.Unmarshal([]byte(y), &wrapper); err != nil {
			return "", fmt.Errorf("input %d: %w", i, err)
		}
		for k, v := range wrapper.Roles {
			roles[k] = v
		}
	}

	if len(roles) == 0 {
		return "", nil
	}
	out, _ := yaml.Marshal(map[string]any{"roles": roles})
	return string(out), nil
}

// composePipelineTemplates serializes the delivered pipeline-template
// documents under a single templates key, in input order (design D7). The
// caller has already resolved the identity-collision rule from D5, so docs
// carries no duplicate name+version pair.
func composePipelineTemplates(docs []pipelinetemplate.Document) (string, error) {
	if len(docs) == 0 {
		return "", nil
	}
	out, err := yaml.Marshal(struct {
		Templates []pipelinetemplate.Document `yaml:"templates"`
	}{Templates: docs})
	if err != nil {
		return "", fmt.Errorf("marshal pipeline templates: %w", err)
	}
	return string(out), nil
}

// validateItemTemplateValues finds every items-declared
// definition.varroaTemplate in itemsYAML, at any nesting depth under a
// folder, and validates its values against the schema of the template it
// resolves to in delivered (design D10). A template an items-declared
// definition cannot resolve, or whose values fail the schema, is a
// composition error — unlike a marker, an items-declared template is
// fully visible at composition time, so this check need not wait for L3
// at run start.
func validateItemTemplateValues(itemsYAML string, delivered []pipelinetemplate.Document) []string {
	if strings.TrimSpace(itemsYAML) == "" {
		return nil
	}
	var wrapper struct {
		Items []map[string]any `yaml:"items"`
	}
	if err := yaml.Unmarshal([]byte(itemsYAML), &wrapper); err != nil {
		// Malformed itemsYAML is already reported by composeItems' caller.
		return nil
	}
	return validateItemTemplateValuesAt(wrapper.Items, "", delivered)
}

// validateItemTemplateValuesAt validates every item in items and recurses
// into each item's own nested items list, so a pipeline declared inside a
// folder gets the same check as a root-level one. path is the parent
// item's full path, empty at the top level.
func validateItemTemplateValuesAt(items []map[string]any, path string, delivered []pipelinetemplate.Document) []string {
	var errs []string
	for _, it := range items {
		itemName, _ := it["name"].(string)
		itemPath := itemName
		if path != "" {
			itemPath = path + "/" + itemName
		}

		if def, ok := it["definition"].(map[string]any); ok {
			if vt, ok := def["varroaTemplate"].(map[string]any); ok {
				name, _ := vt["template"].(string)
				constraint, _ := vt["templateVersion"].(string)
				values, _ := vt["values"].(map[string]any)

				doc, err := pipelinetemplate.ResolveTemplate(delivered, name, constraint)
				if err != nil {
					errs = append(errs, fmt.Sprintf("item %q: definition.varroaTemplate: %v", itemPath, err))
				} else {
					_, findings, err := pipelinetemplate.ValidateValues(doc.Spec.Parameters, values)
					if err != nil {
						errs = append(errs, fmt.Sprintf(
							"item %q: definition.varroaTemplate %q: validate values: %v", itemPath, name, err))
					}
					for _, f := range findings {
						errs = append(errs, fmt.Sprintf("item %q: definition.varroaTemplate %q: %s", itemPath, name, f.String()))
					}
				}
			}
		}

		nested, ok := it["items"].([]any)
		if !ok {
			continue
		}
		nestedMaps := make([]map[string]any, 0, len(nested))
		for _, n := range nested {
			if m, ok := n.(map[string]any); ok {
				nestedMaps = append(nestedMaps, m)
			}
		}
		errs = append(errs, validateItemTemplateValuesAt(nestedMaps, itemPath, delivered)...)
	}
	return errs
}

// computeResolvedHash computes a SHA256 hash over the concatenation of all
// YAML strings and the sorted key=value variable pairs.
func computeResolvedHash(jenkins, plugins, items, rbac, pipelineTemplates string, vars Variables) string {
	h := sha256.New()
	h.Write([]byte(jenkins))
	h.Write([]byte(plugins))
	h.Write([]byte(items))
	h.Write([]byte(rbac))
	h.Write([]byte(pipelineTemplates))

	// Sort variable keys for deterministic ordering.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, vars[k])
	}

	return hex.EncodeToString(h.Sum(nil))
}
