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

// registry maps Go CRD types to their GVR, kind, and scope. It is the only
// place a new kind registers. A kind that also needs a deep Kubernetes
// operation carries a second GVR variable in
// internal/controller/clientset_client.go; a kind this store alone serves
// does not.
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
	reflect.TypeOf(v1alpha1.Runner{}):                {runnerGVR, "Runner", false},
	reflect.TypeOf(v1alpha1.UpdateCenter{}):          {updateCentersGVR, "UpdateCenter", false},
	reflect.TypeOf(v1alpha1.ProfileCandidate{}):      {profileCandidateGVR, "ProfileCandidate", false},
	reflect.TypeOf(v1alpha1.CellPool{}):              {cellPoolGVR, "CellPool", false},
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
	// gaps (patched to [] when coverage is complete) and resolvedMetadataSources
	// (cleared when pull-through is disabled) carry the same latent defect and
	// clear to nil the same way. On a failed inventory fetch the reconciler
	// returns before reassigning gaps, so the deep-copied previous value is still
	// marshaled and present in the patch — this loop only fires for an absent
	// key, so a present-but-unchanged gaps list is left alone.
	//
	// pluginCount and storeBytes are int/int64 fields whose true zero value is
	// indistinguishable from omitempty's "absent" on the wire, so they need the
	// same set-to-zero handling catalogSourceGVR's itemCount uses: an emptied
	// store must be able to report zero plugins and zero bytes rather than the
	// last nonzero reading forever.
	updateCentersGVR: {
		"seedImportedDigests":     nil,
		"gaps":                    nil,
		"unservable":              nil,
		"resolvedMetadataSources": nil,
		"gapCount":                0,
		"unservableCount":         0,
		"pluginCount":             0,
		"storeBytes":              0,
	},
	// placement, pipelines, and rehydration are all omitempty. A plain
	// marshal leaves the stale cell binding, the door token and secret, or
	// the stale rehydration state in etcd once a Team stops being a
	// pipelines tenant. These three entries clear them, so a re-enabled
	// tenant never revives credentials issued before the teardown.
	// This map holds no controllerGVR entry for "cell". Such an entry
	// would apply to every status patch for the kind and delete the tenant
	// list on the controller reconciler's next full-status patch.
	// staleAgentServiceNamespaces is omitempty too. The tick that deletes
	// the last agent Service a move left behind must be able to empty the
	// list, or the merge patch drops the key, the server keeps the stale
	// names, and every later tick deletes an absent Service again.
	// The three pendingPurge keys inside pipelines need their own entry.
	// The move-purge retry writes their zero values into a pipelines object
	// that is still present, so a plain marshal drops the keys, the server
	// keeps the old values, and the tick repeats a completed purge for
	// good. A NestedClear still deletes the whole object when the caller
	// omits it.
	teamGVR: {
		"placement": nil,
		"pipelines": NestedClear{
			"pendingPurgeGeneration":    nil,
			"pendingPurgeCell":          nil,
			"pendingPurgeCellNamespace": nil,
		},
		"rehydration":                 nil,
		"staleAgentServiceNamespaces": nil,
	},
	// cell and cellNamespace are both omitempty. An unplaced runner must
	// clear both, or a merge patch would leave the stale cell binding in
	// etcd once the owning Team loses its placement.
	runnerGVR: {"cell": "", "cellNamespace": ""},
	// tenants is omitempty. A pool whose last tenant leaves must report an
	// empty list, or a merge patch leaves the stale names in etcd.
	cellPoolGVR: {"tenants": nil},
	// federation is omitempty. A binding whose last hive leaves membership
	// must report an empty list, or a merge patch leaves the stale entries.
	varroaRoleBindingGVR: {"federation": nil},
	// federation is omitempty. A role whose last stale-delete fault clears
	// must report an empty list, or a merge patch leaves the stale entries.
	varroaRoleGVR: {"federation": nil},
}

// NestedClear is a clearStatusFields value for a status object that holds
// clearable keys of its own. The object itself still goes when the caller
// omits it, exactly as a nil value makes it go. When the caller sends the
// object, each key named here that the object omits takes its zero value,
// so a merge patch clears the server's copy of that key too.
type NestedClear map[string]any

// ApplyStatusClears sets every clearable key of gvr that statusMap omits to
// its explicit zero value, so a status merge patch clears the server's
// stale value (RFC 7386: null deletes the key; ""/0 set to zero, matching
// the pre-crdstore per-kind patch bytes exactly). It is the one
// implementation of that rule: ClientsetClient.PatchObjectStatus calls it
// on every status patch it sends.
func ApplyStatusClears(gvr schema.GroupVersionResource, statusMap map[string]any) {
	for key, zero := range clearStatusFields[gvr] {
		nested, isNested := zero.(NestedClear)
		if !isNested {
			if _, sent := statusMap[key]; !sent {
				statusMap[key] = zero
			}
			continue
		}
		child, sent := statusMap[key].(map[string]any)
		if !sent {
			statusMap[key] = nil
			continue
		}
		for name, childZero := range nested {
			if _, sent := child[name]; !sent {
				child[name] = childZero
			}
		}
	}
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

// GVR variables. A name that also appears in
// internal/controller/clientset_client.go must hold the same resource
// string there.
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
	runnerGVR                = gvr("runners")
	jenkinsVersionProfileGVR = gvr("jenkinsversionprofiles")
	controllerClassGVR       = gvr("controllerclasses")
	updateCentersGVR         = gvr("updatecenters")
	profileCandidateGVR      = gvr("profilecandidates")
	cellPoolGVR              = gvr("cellpools")
)

func gvr(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: resource}
}
