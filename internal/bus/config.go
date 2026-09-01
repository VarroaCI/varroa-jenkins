package bus

import "encoding/json"

// OperatorBundlesSubject returns operator.<cluster>.bundles.<verb>.
// verb ∈ list|get|create|update|delete|preview|validate|pause|resume.
// Request/response types per verb are defined below in this file.
func OperatorBundlesSubject(cluster, verb string) string {
	return "operator." + cluster + ".bundles." + verb
}

// OperatorCatalogSubject returns operator.<cluster>.catalog.<verb>.
// verb ∈ itemlist|itemget|sourcelist|sourceget|sourcecreate|sourceupdate|
//
//	sourcedelete|sourcesync.
//
// Request/response types per verb are defined below in this file.
func OperatorCatalogSubject(cluster, verb string) string {
	return "operator." + cluster + ".catalog." + verb
}

// OperatorRbacSubject returns operator.<cluster>.rbac.<verb>.
// verb ∈ rolelist|roleget|rolecreate|roleupdate|roledelete|
//
//	bindinglist|bindingget|bindingcreate|bindingupdate|bindingdelete.
//
// Request/response types per verb are defined below in this file.
func OperatorRbacSubject(cluster, verb string) string {
	return "operator." + cluster + ".rbac." + verb
}

// OperatorProvisioningDefaultsSubject returns operator.<cluster>.provisioningdefaults.<verb>.
// verb ∈ get|update.
func OperatorProvisioningDefaultsSubject(cluster, verb string) string {
	return "operator." + cluster + ".provisioningdefaults." + verb
}

// OperatorVersionProfilesSubject returns operator.<cluster>.versionprofiles.<verb>.
// verb ∈ list|get|create|update|delete|view.
func OperatorVersionProfilesSubject(cluster, verb string) string {
	return "operator." + cluster + ".versionprofiles." + verb
}

// ---------------------------------------------------------------------------
// Generic request/response types (DP1). The same types serve every config kind;
// the subject encodes the kind.
// ---------------------------------------------------------------------------

// ConfigListRequest is the payload for *list subjects (bundles.list,
// catalog.itemlist, catalog.sourcelist, rbac.rolelist, rbac.bindinglist).
// Namespace is ignored for cluster-scoped kinds (JenkinsRole, JenkinsRoleBinding).
type ConfigListRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// ConfigListResponse is the reply for *list subjects.
// OperatorNamespace is populated only for catalog.itemlist — the BFF echoes it
// in the catalog-items HTTP response as operatorNamespace.
// Empty for other kinds.
type ConfigListResponse struct {
	Items             []json.RawMessage `json:"items,omitempty"`
	OperatorNamespace string            `json:"operatorNamespace,omitempty"`
	Code              string            `json:"code,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// CatalogItemSummary is the per-item shape returned by catalog.itemlist.
// It intentionally omits status.content so the list reply stays under the
// 900KB request-reply budget. Full detail is available via catalog.itemget.
type CatalogItemSummary struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	DisplayName string   `json:"displayName,omitempty"`
	Type        string   `json:"type"`
	SourceRef   string   `json:"sourceRef"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Valid       bool     `json:"valid"`
	Message     string   `json:"message,omitempty"`
	ContentHash string   `json:"contentHash,omitempty"`
	// Variables carries the item's declared variables so list consumers (the
	// wizard's typed variable widgets) don't need a per-item detail fetch.
	Variables []CatalogVariableSummary `json:"variables,omitempty"`
	// PluginName is the update-center plugin an item was derived for. The
	// browser collapses rows by it, so a plugin stored at three versions is one
	// row with a version selector rather than three rows. Empty for every item
	// that is not update-center-derived.
	PluginName string `json:"pluginName,omitempty"`
	// Compat carries the item's advisory per-profile verdicts so the browser can
	// badge a row without a per-item detail fetch. Advisory only: a badge never
	// disables selection.
	Compat []CatalogItemCompatSummary `json:"compat,omitempty"`
}

// CatalogItemCompatSummary mirrors v1alpha1.CatalogItemCompat for list replies;
// bus stays free of the API-types dependency.
type CatalogItemCompatSummary struct {
	Profile        string `json:"profile"`
	JenkinsVersion string `json:"jenkinsVersion,omitempty"`
	Verdict        string `json:"verdict"`
	Message        string `json:"message,omitempty"`
}

// CatalogVariableSummary mirrors v1alpha1.CatalogVariable for list replies;
// bus stays free of the API-types dependency.
type CatalogVariableSummary struct {
	Name          string   `json:"name"`
	Default       string   `json:"default,omitempty"`
	Description   string   `json:"description,omitempty"`
	Required      bool     `json:"required,omitempty"`
	Type          string   `json:"type,omitempty"`
	AllowedValues []string `json:"allowedValues,omitempty"`
}

// ConfigGetRequest is the payload for *get subjects (bundles.get, catalog.itemget,
// catalog.sourceget, rbac.roleget, rbac.bindingget).
type ConfigGetRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ConfigGetResponse is the reply for *get subjects.
type ConfigGetResponse struct {
	Item  json.RawMessage `json:"item,omitempty"`
	Code  string          `json:"code,omitempty"`
	Error string          `json:"error,omitempty"`
}

// ConfigApplyRequest is the payload for create and update subjects
// (bundles.create, bundles.update, catalog.sourcecreate, catalog.sourceupdate,
// rbac.rolecreate, rbac.roleupdate, rbac.bindingcreate, rbac.bindingupdate).
// Object carries the full CR body (apiVersion/kind/metadata/spec).
type ConfigApplyRequest struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Object    json.RawMessage `json:"object"`
}

// ConfigApplyResponse is the reply for create and update subjects.
type ConfigApplyResponse struct {
	Item  json.RawMessage `json:"item,omitempty"`
	Code  string          `json:"code,omitempty"`
	Error string          `json:"error,omitempty"`
}

// ConfigDeleteRequest is the payload for *delete subjects (bundles.delete,
// catalog.sourcedelete, rbac.roledelete, rbac.bindingdelete).
type ConfigDeleteRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ConfigDeleteResponse is the reply for *delete subjects.
type ConfigDeleteResponse struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// VersionProfileView is one JenkinsVersionProfile CR enriched with resolved
// plugin lines from the target operator namespace.
type VersionProfileView struct {
	Item            json.RawMessage `json:"item"`
	ResolvedPlugins []string        `json:"resolvedPlugins,omitempty"`
	ResolveVersion  string          `json:"resolveVersion,omitempty"`
}

// VersionProfileViewResponse is the reply for versionprofiles.view subjects.
type VersionProfileViewResponse struct {
	Profiles []VersionProfileView `json:"profiles,omitempty"`
	Code     string               `json:"code,omitempty"`
	Error    string               `json:"error,omitempty"`
}

// BundlePauseRequest is the payload for bundles.pause and bundles.resume.
// Paused=true means pause; Paused=false means resume.
type BundlePauseRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Paused    bool   `json:"paused"`
}

// BundlePauseResponse is the reply for bundles.pause and bundles.resume.
type BundlePauseResponse struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// BundleComposeRequest is the payload for bundles.preview and bundles.validate.
type BundleComposeRequest struct {
	Namespace string          `json:"namespace"`
	Spec      json.RawMessage `json:"spec"`
}

// BundleComposeResponse is the reply for bundles.preview and bundles.validate.
type BundleComposeResponse struct {
	Preview *BundleComposePreview `json:"preview,omitempty"`
	Code    string                `json:"code,omitempty"`
	Error   string                `json:"error,omitempty"`
}

// BundleComposePreview carries the full preview output from the composer.
// All YAML fields are rendered strings; Missing, Drifted, Warnings,
// UnresolvedVariables, and Errors are informational lists.
type BundleComposePreview struct {
	BundleYAML          string             `json:"bundleYaml"`
	JenkinsYAML         string             `json:"jenkinsYaml"`
	PluginsYAML         string             `json:"pluginsYaml"`
	ItemsYAML           string             `json:"itemsYaml"`
	RbacYAML            string             `json:"rbacYaml"`
	Missing             []string           `json:"missing"`
	Drifted             []string           `json:"drifted"`
	Warnings            []string           `json:"warnings"`
	UnresolvedVariables []string           `json:"unresolvedVariables"`
	Errors              []string           `json:"errors,omitempty"`
	PinPreflight        PinPreflightReport `json:"pinPreflight"`
}

// PinConflict is a bundle-pinned plugin whose version differs from the
// resolved set's version for the same artifact.
type PinConflict struct {
	ArtifactID    string `json:"artifactId"`
	BundleVersion string `json:"bundleVersion"`
	SetVersion    string `json:"setVersion"`
}

// MissingPlugin is a bundle-pinned plugin whose artifact ID is absent from
// the resolved set. Advisory only — never a conflict.
type MissingPlugin struct {
	ArtifactID    string `json:"artifactId"`
	BundleVersion string `json:"bundleVersion"`
}

// PinPreflightReport is the result of comparing a bundle's plugin pins
// against a resolved plugin set. bus does not import internal/bundle, so this
// mirrors bundle.PinPreflightReport as the wire shape.
type PinPreflightReport struct {
	Conflicts []PinConflict   `json:"conflicts"`
	Missing   []MissingPlugin `json:"missing"`
}

// CatalogSyncRequest is the payload for catalog.sourcesync.
type CatalogSyncRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// CatalogSyncResponse is the reply for catalog.sourcesync.
type CatalogSyncResponse struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}
