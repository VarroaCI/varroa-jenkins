package v1alpha1

import "k8s.io/apimachinery/pkg/runtime/schema"

// CRD kind constants.
const (
	ControllerKind            = "Controller"
	VarroaRoleKind            = "VarroaRole"
	VarroaRoleBindingKind     = "VarroaRoleBinding"
	JenkinsRoleKind           = "JenkinsRole"
	JenkinsRoleBindingKind    = "JenkinsRoleBinding"
	UserKind                  = "User"
	GroupKind                 = "Group"
	PodTemplateKind           = "PodTemplate"
	BuildMetricKind           = "BuildMetric"
	CatalogSourceKind         = "CatalogSource"
	CatalogItemKind           = "CatalogItem"
	ComposedBundleKind        = "ComposedBundle"
	ProvisioningDefaultsKind  = "ProvisioningDefaults"
	JenkinsVersionProfileKind = "JenkinsVersionProfile"
	UpdateCenterKind          = "UpdateCenter"
	ControllerClassKind       = "ControllerClass"
	TeamKind                  = "Team"
)

// GroupVersionKind instances for each CRD kind.
var (
	ControllerGVK            = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: ControllerKind}
	VarroaRoleGVK            = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: VarroaRoleKind}
	VarroaRoleBindingGVK     = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: VarroaRoleBindingKind}
	JenkinsRoleGVK           = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: JenkinsRoleKind}
	JenkinsRoleBindingGVK    = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: JenkinsRoleBindingKind}
	UserGVK                  = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: UserKind}
	GroupGVK                 = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: GroupKind}
	PodTemplateGVK           = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: PodTemplateKind}
	BuildMetricGVK           = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: BuildMetricKind}
	CatalogSourceGVK         = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: CatalogSourceKind}
	CatalogItemGVK           = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: CatalogItemKind}
	ComposedBundleGVK        = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: ComposedBundleKind}
	ProvisioningDefaultsGVK  = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: ProvisioningDefaultsKind}
	JenkinsVersionProfileGVK = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: JenkinsVersionProfileKind}
	UpdateCenterGVK          = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: UpdateCenterKind}
	ControllerClassGVK       = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: ControllerClassKind}
	TeamGVK                  = schema.GroupVersionKind{Group: GroupName, Version: "v1alpha1", Kind: TeamKind}
)
