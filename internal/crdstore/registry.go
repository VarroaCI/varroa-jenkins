package crdstore

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

type kindInfo struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
}

// registry maps Go CRD types to their GVR, kind, and scope. Every entry must
// match a GVR variable declared at the top of internal/controller/clientset_client.go.
var registry = map[reflect.Type]kindInfo{
	// Namespaced kinds.
	reflect.TypeOf(v1alpha1.Controller{}):     {controllerGVR, "Controller", true},
	reflect.TypeOf(v1alpha1.PodTemplate{}):    {podTemplateGVR, "PodTemplate", true},
	reflect.TypeOf(v1alpha1.CatalogSource{}):  {catalogSourceGVR, "CatalogSource", true},
	reflect.TypeOf(v1alpha1.CatalogItem{}):    {catalogItemGVR, "CatalogItem", true},
	reflect.TypeOf(v1alpha1.ComposedBundle{}): {composedBundleGVR, "ComposedBundle", true},
	reflect.TypeOf(v1alpha1.BroodOperation{}): {broodOperationGVR, "BroodOperation", true},
	reflect.TypeOf(v1alpha1.User{}):           {userGVR, "User", true},

	// Cluster-scoped kinds.
	reflect.TypeOf(v1alpha1.VarroaRole{}):            {varroaRoleGVR, "VarroaRole", false},
	reflect.TypeOf(v1alpha1.VarroaRoleBinding{}):     {varroaRoleBindingGVR, "VarroaRoleBinding", false},
	reflect.TypeOf(v1alpha1.JenkinsRole{}):           {jenkinsRoleGVR, "JenkinsRole", false},
	reflect.TypeOf(v1alpha1.JenkinsRoleBinding{}):    {jenkinsRoleBindingGVR, "JenkinsRoleBinding", false},
	reflect.TypeOf(v1alpha1.ProvisioningDefaults{}):  {provisioningDefaultsGVR, "ProvisioningDefaults", false},
	reflect.TypeOf(v1alpha1.ControllerClass{}):       {controllerClassGVR, "ControllerClass", false},
	reflect.TypeOf(v1alpha1.JenkinsVersionProfile{}): {jenkinsVersionProfileGVR, "JenkinsVersionProfile", false},
	reflect.TypeOf(v1alpha1.Group{}):                 {groupGVR, "Group", false},
	reflect.TypeOf(v1alpha1.Team{}):                  {teamGVR, "Team", false},
	reflect.TypeOf(v1alpha1.UpdateCenter{}):          {updateCentersGVR, "UpdateCenter", false},
}

// clearStatusFields lists, per resource, the status keys a merge patch must
// clear when the caller's status omits them (the fields are omitempty, so a
// plain marshal would silently leave stale server-side values). Curated to
// match the pre-crdstore per-kind patch methods exactly — do NOT generalize:
// most kinds (User, Team, …) rely on partial patches leaving unrelated
// fields (credentials, preferences) untouched.
var clearStatusFields = map[schema.GroupVersionResource]map[string]any{
	composedBundleGVR: {"errors": nil, "warnings": nil, "message": ""},
	catalogSourceGVR:  {"message": "", "itemCount": 0},
	// contentHash and message clear alongside content: a derived item that goes
	// invalid must not keep advertising the old hash, and one that recovers must
	// not keep showing the failure text next to status.valid=true.
	catalogItemGVR: {"content": "", "contentHash": "", "message": "", "closure": nil, "compat": nil, "conditions": nil},
	// seedImportedDigests must be able to go empty: the spec requires it patched
	// every tick "including as []", so a ref removed from spec.seed.refs stops
	// being tracked. It is omitempty, and the reconciler always patches a full
	// status (a deep copy of the previous one), so the key is absent only when it
	// is genuinely meant to be empty. Without this, the merge patch drops the key
	// and the server keeps the stale list — after which a re-added ref at the same
	// digest is skipped forever as "already imported" even if the store was wiped
	// in between.
	//
	// Two other UpdateCenter status fields have the same latent defect and are NOT
	// fixed here: gaps (spec: patched to [] when coverage is complete) and
	// resolvedMetadataSources (cleared when pull-through is disabled). Both are
	// omitempty and neither can currently reach empty on the server. They belong to
	// different features, gaps has a retain-on-failure rule that needs its own
	// reasoning, and each wants its own regression test — filed as #551 rather than
	// bundled in blind.
	updateCentersGVR: {"seedImportedDigests": nil},
}

// ClearStatusFields returns, per status key, the explicit zero value a
// status merge patch must carry for gvr when the key is absent from the
// marshaled status (nil → RFC 7386 key deletion; ""/0 → set-to-zero,
// matching the pre-crdstore per-kind patch bytes exactly).
func ClearStatusFields(gvr schema.GroupVersionResource) map[string]any {
	return clearStatusFields[gvr]
}

// GVRFor returns the schema.GroupVersionResource for the given type T.
// Returns an error when T is not registered.
func GVRFor[T any]() (schema.GroupVersionResource, error) {
	var zero T
	info, ok := registry[reflect.TypeOf(zero)]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("crdstore: type %T not registered", zero)
	}
	return info.gvr, nil
}

// gvrInfo returns the full kindInfo for the given type T.
func gvrInfo[T any]() (kindInfo, error) {
	var zero T
	info, ok := registry[reflect.TypeOf(zero)]
	if !ok {
		return kindInfo{}, fmt.Errorf("crdstore: type %T not registered", zero)
	}
	return info, nil
}

// GVR variables — mirrors the source-of-truth declarations in
// internal/controller/clientset_client.go:36-52.
var (
	controllerGVR            = gvr("controllers")
	varroaRoleGVR            = gvr("varroaroles")
	varroaRoleBindingGVR     = gvr("varroarolebindings")
	jenkinsRoleGVR           = gvr("jenkinsroles")
	jenkinsRoleBindingGVR    = gvr("jenkinsrolebindings")
	podTemplateGVR           = gvr("podtemplates")
	catalogSourceGVR         = gvr("catalogsources")
	catalogItemGVR           = gvr("catalogitems")
	composedBundleGVR        = gvr("composedbundles")
	broodOperationGVR        = gvr("broodoperations")
	provisioningDefaultsGVR  = gvr("provisioningdefaults")
	userGVR                  = gvr("users")
	groupGVR                 = gvr("groups")
	teamGVR                  = gvr("teams")
	jenkinsVersionProfileGVR = gvr("jenkinsversionprofiles")
	controllerClassGVR       = gvr("controllerclasses")
	updateCentersGVR         = gvr("updatecenters")
)

func gvr(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: resource}
}
