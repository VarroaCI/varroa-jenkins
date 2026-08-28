package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/profileview"
)

// ConfigCRUDClient is a narrow interface for the k8s client operations
// that ConfigCRUD needs, allowing testability without *ClientsetClient.
type ConfigCRUDClient interface {
	GetConfigMap(ctx context.Context, name, namespace string) (map[string]string, error)
	GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error)
	GetSecretAnnotations(ctx context.Context, name, namespace string) (map[string]string, error)
}

// ConfigCRUD implements operator-side CRUD handlers for BFF-initiated config
// operations received over the NATS bus. Each Handle* method unmarshals the
// request, validates against local k8s state, and returns a JSON-marshalled
// response (DP2).
type ConfigCRUD struct {
	Client            ConfigCRUDClient
	Store             crdstore.Backend
	Composer          *bundle.Composer
	OperatorNamespace string
	Logger            *slog.Logger
}

// NewConfigCRUD creates a new ConfigCRUD.
func NewConfigCRUD(client ConfigCRUDClient, store crdstore.Backend, composer *bundle.Composer, operatorNamespace string, logger *slog.Logger) *ConfigCRUD {
	return &ConfigCRUD{
		Client:            client,
		Store:             store,
		Composer:          composer,
		OperatorNamespace: operatorNamespace,
		Logger:            logger,
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// configReply marshals v to JSON for the bus response.
func (c *ConfigCRUD) configReply(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		c.Logger.Error("config reply marshal failed", "error", err)
		errResp, _ := json.Marshal(map[string]string{"error": "internal marshal error", "code": bus.CodeInternal})
		return errResp
	}
	return data
}

// stripUnstructuredManagedFields clears managedFields on an unstructured object.
func stripUnstructuredManagedFields(u map[string]any) {
	if meta, ok := u["metadata"].(map[string]any); ok {
		delete(meta, "managedFields")
	}
}

// marshalCR strips managedFields and marshals a CR to json.RawMessage.
func marshalCR(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Unmarshal into a generic map, strip managedFields, re-marshal.
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	stripUnstructuredManagedFields(obj)
	return json.Marshal(obj)
}

// ProjectCatalogItemSummary projects a CatalogItem CR to its list-reply summary.
func ProjectCatalogItemSummary(cr *v1alpha1.CatalogItem) bus.CatalogItemSummary {
	vars := make([]bus.CatalogVariableSummary, 0, len(cr.Spec.Variables))
	for _, v := range cr.Spec.Variables {
		vars = append(vars, bus.CatalogVariableSummary{
			Name:          v.Name,
			Default:       v.Default,
			Description:   v.Description,
			Required:      v.Required,
			Type:          v.Type,
			AllowedValues: v.AllowedValues,
		})
	}
	compat := make([]bus.CatalogItemCompatSummary, 0, len(cr.Status.Compat))
	for _, c := range cr.Status.Compat {
		compat = append(compat, bus.CatalogItemCompatSummary{
			Profile:        c.Profile,
			JenkinsVersion: c.JenkinsVersion,
			Verdict:        c.Verdict,
			Message:        c.Message,
		})
	}
	if len(compat) == 0 {
		compat = nil
	}
	return bus.CatalogItemSummary{
		Name:        cr.Name,
		Namespace:   cr.Namespace,
		DisplayName: cr.Spec.DisplayName,
		Type:        string(cr.Spec.Type),
		SourceRef:   cr.Spec.SourceRef,
		Version:     cr.Spec.Version,
		Description: cr.Spec.Description,
		Tags:        cr.Spec.Tags,
		Valid:       cr.Status.Valid,
		Message:     cr.Status.Message,
		ContentHash: cr.Status.ContentHash,
		Variables:   vars,
		PluginName:  catalogItemPluginName(cr),
		Compat:      compat,
	}
}

// catalogItemPluginName recovers the plugin an update-center item was derived
// for. The label is authoritative when present; spec.path carries the same fact
// as "uc://<name>@<version>" and covers a plugin whose name does not survive
// slugging into a label value.
func catalogItemPluginName(cr *v1alpha1.CatalogItem) string {
	if cr.Spec.SourceRef != updateCenterSourceName {
		return ""
	}
	if v := cr.Labels[pluginNameLabel]; v != "" {
		return v
	}
	rest, ok := strings.CutPrefix(cr.Spec.Path, "uc://")
	if !ok {
		return ""
	}
	if at := strings.LastIndex(rest, "@"); at > 0 {
		return rest[:at]
	}
	return rest
}

// listBudgetReply marshals items; if the result exceeds 900KB, returns
// a code:internal response with "list too large".
func (c *ConfigCRUD) listBudgetReply(items []json.RawMessage, operatorNamespace string) []byte {
	reply := bus.ConfigListResponse{Items: items, OperatorNamespace: operatorNamespace}
	data, err := json.Marshal(reply)
	if err != nil {
		c.Logger.Error("list reply marshal failed", "error", err)
		return c.configReply(bus.ConfigListResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if len(data) > 900*1024 {
		return c.configReply(bus.ConfigListResponse{Code: bus.CodeInternal, Error: "list too large"})
	}
	return data
}

// k8sConfigErrToCode maps k8s err to bus code.
func k8sConfigErrToCode(err error) string {
	if apierrors.IsNotFound(err) {
		return bus.CodeNotFound
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return bus.CodeConflict
	}
	return bus.CodeInternal
}

// ---------------------------------------------------------------------------
// Bundles list/get/delete
// ---------------------------------------------------------------------------

// HandleBundlesList handles operator.<cluster>.bundles.list.
func (c *ConfigCRUD) HandleBundlesList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.ComposedBundle](ctx, c.Store, req.Namespace, "")
	if err != nil {
		c.Logger.Error("list bundles failed", "namespace", req.Namespace, "error", err)
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			c.Logger.Error("marshal bundle failed", "name", cr.Name, "error", err)
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	return c.listBudgetReply(rawItems, "")
}

// HandleBundlesGet handles operator.<cluster>.bundles.get.
func (c *ConfigCRUD) HandleBundlesGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.ConfigGetResponse{Error: "bundle not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get bundle failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleBundlesDelete handles operator.<cluster>.bundles.delete.
func (c *ConfigCRUD) HandleBundlesDelete(data []byte) []byte {
	var req bus.ConfigDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := crdstore.Delete[v1alpha1.ComposedBundle](ctx, c.Store, req.Name, req.Namespace); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{
			Error: fmt.Sprintf("failed to delete bundle: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	return c.configReply(bus.ConfigDeleteResponse{})
}

// ---------------------------------------------------------------------------
// CatalogSources list/get/delete
// ---------------------------------------------------------------------------

// HandleSourcesList handles operator.<cluster>.catalog.sourcelist.
func (c *ConfigCRUD) HandleSourcesList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.CatalogSource](ctx, c.Store, req.Namespace, "")
	if err != nil {
		c.Logger.Error("list catalogsources failed", "namespace", req.Namespace, "error", err)
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			c.Logger.Error("marshal catalogsource failed", "name", cr.Name, "error", err)
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	return c.listBudgetReply(rawItems, "")
}

// HandleSourcesGet handles operator.<cluster>.catalog.sourceget.
func (c *ConfigCRUD) HandleSourcesGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.CatalogSource](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.ConfigGetResponse{Error: "catalogsource not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get catalogsource failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleSourcesDelete handles operator.<cluster>.catalog.sourcedelete.
func (c *ConfigCRUD) HandleSourcesDelete(data []byte) []byte {
	var req bus.ConfigDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := crdstore.Delete[v1alpha1.CatalogSource](ctx, c.Store, req.Name, req.Namespace); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{
			Error: fmt.Sprintf("failed to delete catalogsource: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	return c.configReply(bus.ConfigDeleteResponse{})
}

// ---------------------------------------------------------------------------
// CatalogItems list/get (read-only — no delete for items)
// ---------------------------------------------------------------------------

// HandleItemsList handles operator.<cluster>.catalog.itemlist.
func (c *ConfigCRUD) HandleItemsList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.CatalogItem](ctx, c.Store, req.Namespace, "")
	if err != nil {
		c.Logger.Error("list catalogitems failed", "namespace", req.Namespace, "error", err)
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(ProjectCatalogItemSummary(cr))
		if err != nil {
			c.Logger.Error("marshal catalogitem failed", "name", cr.Name, "error", err)
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	return c.listBudgetReply(rawItems, c.OperatorNamespace)
}

// HandleItemsGet handles operator.<cluster>.catalog.itemget.
func (c *ConfigCRUD) HandleItemsGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.CatalogItem](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.ConfigGetResponse{Error: "catalogitem not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get catalogitem failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// ---------------------------------------------------------------------------
// JenkinsRoles list/get/delete (cluster-scoped)
// ---------------------------------------------------------------------------

// HandleRolesList handles operator.<cluster>.rbac.rolelist.
func (c *ConfigCRUD) HandleRolesList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.JenkinsRole](ctx, c.Store, "", "")
	if err != nil {
		c.Logger.Error("list jenkinsroles failed", "error", err)
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			c.Logger.Error("marshal jenkinsrole failed", "name", cr.Name, "error", err)
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	return c.listBudgetReply(rawItems, "")
}

// HandleRolesGet handles operator.<cluster>.rbac.roleget.
func (c *ConfigCRUD) HandleRolesGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, c.Store, req.Name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.ConfigGetResponse{Error: "jenkinsrole not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get jenkinsrole failed", "name", req.Name, "error", err)
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleRolesDelete handles operator.<cluster>.rbac.roledelete.
func (c *ConfigCRUD) HandleRolesDelete(data []byte) []byte {
	var req bus.ConfigDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := crdstore.Delete[v1alpha1.JenkinsRole](ctx, c.Store, req.Name, ""); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{
			Error: fmt.Sprintf("failed to delete jenkinsrole: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	return c.configReply(bus.ConfigDeleteResponse{})
}

// ---------------------------------------------------------------------------
// JenkinsRoleBindings list/get/delete (cluster-scoped)
// ---------------------------------------------------------------------------

// HandleBindingsList handles operator.<cluster>.rbac.bindinglist.
func (c *ConfigCRUD) HandleBindingsList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, c.Store, "", "")
	if err != nil {
		c.Logger.Error("list jenkinsrolebindings failed", "error", err)
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			c.Logger.Error("marshal jenkinsrolebinding failed", "name", cr.Name, "error", err)
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	return c.listBudgetReply(rawItems, "")
}

// HandleBindingsGet handles operator.<cluster>.rbac.bindingget.
func (c *ConfigCRUD) HandleBindingsGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, c.Store, req.Name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.ConfigGetResponse{Error: "jenkinsrolebinding not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get jenkinsrolebinding failed", "name", req.Name, "error", err)
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleBindingsDelete handles operator.<cluster>.rbac.bindingdelete.
func (c *ConfigCRUD) HandleBindingsDelete(data []byte) []byte {
	var req bus.ConfigDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, c.Store, req.Name, ""); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{
			Error: fmt.Sprintf("failed to delete jenkinsrolebinding: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	return c.configReply(bus.ConfigDeleteResponse{})
}

// ---------------------------------------------------------------------------
// ProvisioningDefaults + JenkinsVersionProfiles (cluster-scoped)
// ---------------------------------------------------------------------------

// HandleProvisioningDefaultsGet handles operator.<cluster>.provisioningdefaults.get.
func (c *ConfigCRUD) HandleProvisioningDefaultsGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cr, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, c.Store, req.Name, "")
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleProvisioningDefaultsUpdate handles operator.<cluster>.provisioningdefaults.update.
func (c *ConfigCRUD) HandleProvisioningDefaultsUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cr v1alpha1.ProvisioningDefaults
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid provisioningdefaults JSON", Code: bus.CodeInvalid})
	}
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "ProvisioningDefaults"
	if err := crdstore.Apply[v1alpha1.ProvisioningDefaults](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleVersionProfilesList handles operator.<cluster>.versionprofiles.list.
func (c *ConfigCRUD) HandleVersionProfilesList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	items, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, c.Store, "", "")
	if err != nil {
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}
	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			return c.configReply(bus.ConfigListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}
	return c.listBudgetReply(rawItems, "")
}

// HandleVersionProfilesGet handles operator.<cluster>.versionprofiles.get.
func (c *ConfigCRUD) HandleVersionProfilesGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cr, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, c.Store, req.Name, "")
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleVersionProfilesCreate handles operator.<cluster>.versionprofiles.create.
func (c *ConfigCRUD) HandleVersionProfilesCreate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cr v1alpha1.JenkinsVersionProfile
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsversionprofile JSON", Code: bus.CodeInvalid})
	}
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsVersionProfile"
	if err := crdstore.Create[v1alpha1.JenkinsVersionProfile](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleVersionProfilesUpdate handles operator.<cluster>.versionprofiles.update.
func (c *ConfigCRUD) HandleVersionProfilesUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cr v1alpha1.JenkinsVersionProfile
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsversionprofile JSON", Code: bus.CodeInvalid})
	}
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsVersionProfile"
	if err := crdstore.Update[v1alpha1.JenkinsVersionProfile](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleVersionProfilesDelete handles operator.<cluster>.versionprofiles.delete.
func (c *ConfigCRUD) HandleVersionProfilesDelete(data []byte) []byte {
	var req bus.ConfigDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := crdstore.Delete[v1alpha1.JenkinsVersionProfile](ctx, c.Store, req.Name, ""); err != nil {
		return c.configReply(bus.ConfigDeleteResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	return c.configReply(bus.ConfigDeleteResponse{})
}

// HandleVersionProfilesView handles operator.<cluster>.versionprofiles.view.
func (c *ConfigCRUD) HandleVersionProfilesView(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.VersionProfileViewResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	items, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, c.Store, "", "")
	if err != nil {
		return c.configReply(bus.VersionProfileViewResponse{Error: err.Error(), Code: bus.CodeInternal})
	}
	profiles := make([]bus.VersionProfileView, 0, len(items))
	for _, cr := range items {
		raw, err := marshalCR(cr)
		if err != nil {
			return c.configReply(bus.VersionProfileViewResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		view := bus.VersionProfileView{Item: raw, ResolveVersion: cr.Spec.ResolveVersion}
		if cr.Status.ContentRef != "" {
			if cm, err := c.Client.GetConfigMap(ctx, cr.Status.ContentRef, c.OperatorNamespace); err == nil {
				if y := cm["plugins.yaml"]; y != "" {
					if lines, err := profileview.PluginLinesFromYAML(y); err == nil {
						view.ResolvedPlugins = lines
					}
				}
			}
		}
		profiles = append(profiles, view)
	}
	reply := bus.VersionProfileViewResponse{Profiles: profiles}
	replyData, err := json.Marshal(reply)
	if err != nil {
		return c.configReply(bus.VersionProfileViewResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	if len(replyData) > 900*1024 {
		return c.configReply(bus.VersionProfileViewResponse{Error: "list too large", Code: bus.CodeInternal})
	}
	return replyData
}

// ---------------------------------------------------------------------------
// Bundles create/update (Task 2.4)
// ---------------------------------------------------------------------------

// HandleBundlesCreate handles operator.<cluster>.bundles.create.
func (c *ConfigCRUD) HandleBundlesCreate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.ComposedBundle
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid bundle JSON", Code: bus.CodeInvalid})
	}

	// Force addressing from the request envelope (DP2).
	cr.Namespace = req.Namespace
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "ComposedBundle"

	if err := crdstore.Create[v1alpha1.ComposedBundle](ctx, c.Store, &cr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.configReply(bus.ConfigApplyResponse{Error: "bundle already exists", Code: bus.CodeConflict})
		}
		c.Logger.Error("create bundle failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleBundlesUpdate handles operator.<cluster>.bundles.update.
func (c *ConfigCRUD) HandleBundlesUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.ComposedBundle
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid bundle JSON", Code: bus.CodeInvalid})
	}

	// Force addressing from the request envelope (DP2).
	cr.Namespace = req.Namespace
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "ComposedBundle"

	if err := crdstore.Update[v1alpha1.ComposedBundle](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{
			Error: fmt.Sprintf("failed to update bundle: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// ---------------------------------------------------------------------------
// CatalogSources create/update
// ---------------------------------------------------------------------------

// HandleSourcesCreate handles operator.<cluster>.catalog.sourcecreate.
func (c *ConfigCRUD) HandleSourcesCreate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.CatalogSource
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid catalogsource JSON", Code: bus.CodeInvalid})
	}

	cr.Namespace = req.Namespace
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "CatalogSource"

	if err := crdstore.Create[v1alpha1.CatalogSource](ctx, c.Store, &cr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.configReply(bus.ConfigApplyResponse{Error: "catalogsource already exists", Code: bus.CodeConflict})
		}
		c.Logger.Error("create catalogsource failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleSourcesUpdate handles operator.<cluster>.catalog.sourceupdate.
func (c *ConfigCRUD) HandleSourcesUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.CatalogSource
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid catalogsource JSON", Code: bus.CodeInvalid})
	}

	cr.Namespace = req.Namespace
	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "CatalogSource"

	if err := crdstore.Update[v1alpha1.CatalogSource](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{
			Error: fmt.Sprintf("failed to update catalogsource: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// ---------------------------------------------------------------------------
// JenkinsRoles create/update (cluster-scoped)
// ---------------------------------------------------------------------------

// HandleRolesCreate handles operator.<cluster>.rbac.rolecreate.
func (c *ConfigCRUD) HandleRolesCreate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.JenkinsRole
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsrole JSON", Code: bus.CodeInvalid})
	}

	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsRole"

	if err := crdstore.Create[v1alpha1.JenkinsRole](ctx, c.Store, &cr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.configReply(bus.ConfigApplyResponse{Error: "jenkinsrole already exists", Code: bus.CodeConflict})
		}
		c.Logger.Error("create jenkinsrole failed", "name", req.Name, "error", err)
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleRolesUpdate handles operator.<cluster>.rbac.roleupdate.
func (c *ConfigCRUD) HandleRolesUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.JenkinsRole
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsrole JSON", Code: bus.CodeInvalid})
	}

	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsRole"

	if err := crdstore.Update[v1alpha1.JenkinsRole](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{
			Error: fmt.Sprintf("failed to update jenkinsrole: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// ---------------------------------------------------------------------------
// JenkinsRoleBindings create/update (cluster-scoped)
// ---------------------------------------------------------------------------

// HandleBindingsCreate handles operator.<cluster>.rbac.bindingcreate.
func (c *ConfigCRUD) HandleBindingsCreate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.JenkinsRoleBinding
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsrolebinding JSON", Code: bus.CodeInvalid})
	}

	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsRoleBinding"

	if err := crdstore.Create[v1alpha1.JenkinsRoleBinding](ctx, c.Store, &cr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.configReply(bus.ConfigApplyResponse{Error: "jenkinsrolebinding already exists", Code: bus.CodeConflict})
		}
		c.Logger.Error("create jenkinsrolebinding failed", "name", req.Name, "error", err)
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// HandleBindingsUpdate handles operator.<cluster>.rbac.bindingupdate.
func (c *ConfigCRUD) HandleBindingsUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cr v1alpha1.JenkinsRoleBinding
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid jenkinsrolebinding JSON", Code: bus.CodeInvalid})
	}

	cr.Name = req.Name
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "JenkinsRoleBinding"

	if err := crdstore.Update[v1alpha1.JenkinsRoleBinding](ctx, c.Store, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{
			Error: fmt.Sprintf("failed to update jenkinsrolebinding: %s", err.Error()),
			Code:  k8sConfigErrToCode(err),
		})
	}

	raw, err := marshalCR(&cr)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}

// ---------------------------------------------------------------------------
// Bundles pause/resume (Task 2.5)
// ---------------------------------------------------------------------------

// HandleBundlesPause handles operator.<cluster>.bundles.pause and
// operator.<cluster>.bundles.resume.
func (c *ConfigCRUD) HandleBundlesPause(data []byte) []byte {
	var req bus.BundlePauseRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.BundlePauseResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.BundlePauseResponse{Error: "bundle not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get bundle for pause failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.BundlePauseResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	if req.Paused {
		if cr.Annotations == nil {
			cr.Annotations = make(map[string]string)
		}
		cr.Annotations["varroa.dev/rollout-paused"] = "true"
	} else {
		if cr.Annotations != nil {
			delete(cr.Annotations, "varroa.dev/rollout-paused")
			if len(cr.Annotations) == 0 {
				cr.Annotations = nil
			}
		}
	}

	if err := crdstore.Apply[v1alpha1.ComposedBundle](ctx, c.Store, cr); err != nil {
		c.Logger.Error("apply bundle for pause/resume failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.BundlePauseResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	return c.configReply(bus.BundlePauseResponse{})
}

// HandleSourceSync handles operator.<cluster>.catalog.sourcesync.
func (c *ConfigCRUD) HandleSourceSync(data []byte) []byte {
	var req bus.CatalogSyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.CatalogSyncResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.CatalogSource](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.CatalogSyncResponse{Error: "catalogsource not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get catalogsource for sync failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.CatalogSyncResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	if cr.Annotations == nil {
		cr.Annotations = make(map[string]string)
	}
	cr.Annotations["varroa.dev/sync-requested-at"] = time.Now().UTC().Format(time.RFC3339Nano)

	if err := crdstore.Apply[v1alpha1.CatalogSource](ctx, c.Store, cr); err != nil {
		c.Logger.Error("apply catalogsource for sync failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.configReply(bus.CatalogSyncResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	return c.configReply(bus.CatalogSyncResponse{})
}

// ---------------------------------------------------------------------------
// Bundles preview/validate (Task 2.6) — shared compose helper
// ---------------------------------------------------------------------------

// HandleBundlesPreview handles operator.<cluster>.bundles.preview.
func (c *ConfigCRUD) HandleBundlesPreview(data []byte) []byte {
	return c.composeBundle(data, false)
}

// HandleBundlesValidate handles operator.<cluster>.bundles.validate.
func (c *ConfigCRUD) HandleBundlesValidate(data []byte) []byte {
	return c.composeBundle(data, true)
}

// composeBundle is the shared handler for preview and validate.
// It resolves git auth, runs the composer, and returns BundleComposeResponse
// with errors as data (code:""), never as transport errors.
func (c *ConfigCRUD) composeBundle(data []byte, _ /* validateOnly — the BFF distinguishes by endpoint, operator side always composes */ bool) []byte {
	var req bus.BundleComposeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.BundleComposeResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var spec v1alpha1.ComposedBundleSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return c.configReply(bus.BundleComposeResponse{Error: "invalid spec JSON", Code: bus.CodeInvalid})
	}

	// Resolve git auth from local Secrets (port from internal/api/handlers.go:2672).
	resolvedAuth, gitAuthErrors := c.resolveComposedBundleGitAuth(ctx, req.Namespace, &spec)

	// Resolve OCI auth from local Secrets.
	resolvedOCIAuth, ociAuthErrors := c.resolveComposedBundleOCIAuth(ctx, req.Namespace, &spec)

	composed, err := c.Composer.Compose(ctx, req.Namespace, &spec, resolvedAuth, resolvedOCIAuth)
	if err != nil {
		// Compose errors are data, not transport errors.
		allErrors := append(gitAuthErrors, ociAuthErrors...)
		preview := &bus.BundleComposePreview{
			Errors: append(allErrors, fmt.Sprintf("compose error: %s", err.Error())),
		}
		return c.configReply(bus.BundleComposeResponse{Preview: preview})
	}

	// Build the preview output.
	preview := &bus.BundleComposePreview{
		BundleYAML: composed.BundleYAML,
		Missing:    composed.Missing,
		Drifted:    composed.Drifted,
		Warnings:   composed.Warnings,
	}

	if composed.Materialized != nil {
		preview.JenkinsYAML = composed.Materialized.JenkinsYAML
		preview.PluginsYAML = composed.Materialized.PluginsYAML
		preview.ItemsYAML = composed.Materialized.ItemsYAML
		preview.RbacYAML = composed.Materialized.RbacYAML
	}

	// Scan casc-applied sections (jenkins/rbac — JCasC resolves its own
	// secret-source refs there) separately from the rest, then merge.
	var cascYAML, restYAML string
	for _, s := range []string{preview.JenkinsYAML, preview.RbacYAML} {
		if s != "" {
			cascYAML += s + "\n"
		}
	}
	for _, s := range []string{preview.BundleYAML, preview.PluginsYAML, preview.ItemsYAML} {
		if s != "" {
			restYAML += s + "\n"
		}
	}
	unresolvedSet := make(map[string]bool)
	for _, v := range bundle.FindUnresolvedVariables(cascYAML, spec.Variables, true) {
		unresolvedSet[v] = true
	}
	for _, v := range bundle.FindUnresolvedVariables(restYAML, spec.Variables, false) {
		unresolvedSet[v] = true
	}
	names := make([]string, 0, len(unresolvedSet))
	for v := range unresolvedSet {
		names = append(names, v)
	}
	sort.Strings(names)
	preview.UnresolvedVariables = names

	// Collect errors from git-auth resolution, OCI-auth resolution, and missing-items formatting.
	var errors []string
	errors = append(errors, gitAuthErrors...)
	errors = append(errors, ociAuthErrors...)

	// Format missing-item messages with operator-namespace awareness.
	for _, itemRef := range composed.Missing {
		msg := fmt.Sprintf("itemRef %q: catalog item not found in namespace %q", itemRef, req.Namespace)
		if c.OperatorNamespace != "" && req.Namespace != c.OperatorNamespace {
			msg += fmt.Sprintf(" or operator namespace %q", c.OperatorNamespace)
		}
		errors = append(errors, msg)
	}

	// Add compose errors.
	if composed.Errors != nil {
		errors = append(errors, composed.Errors...)
	}

	preview.Errors = errors

	return c.configReply(bus.BundleComposeResponse{Preview: preview})
}

// resolveComposedBundleGitAuth reads the Secret referenced by each git input
// and returns a map of input index -> GitAuth, so the composer can clone
// private repos during dry-run validate/preview. Inputs without a secretRef
// are left absent (public). Errors NEVER contain secret material — only
// structured messages appended to the errors list.
func (c *ConfigCRUD) resolveComposedBundleGitAuth(ctx context.Context, ns string, spec *v1alpha1.ComposedBundleSpec) (map[int]*bundle.GitAuth, []string) {
	resolved := make(map[int]*bundle.GitAuth)
	var errors []string
	for i, input := range spec.Inputs {
		if input.GitSource == nil || input.GitSource.SecretRef == "" {
			continue
		}
		data, err := c.Client.GetSecret(ctx, input.GitSource.SecretRef, ns)
		if err != nil {
			errors = append(errors, fmt.Sprintf("input[%d]: failed to read git auth secret %q in namespace %q", i, input.GitSource.SecretRef, ns))
			continue
		}
		auth, err := bundle.GitAuthFromSecret(data, "")
		if err != nil {
			errors = append(errors, fmt.Sprintf("input[%d]: bad secret %q: invalid format", i, input.GitSource.SecretRef))
			continue
		}

		// Enforce host-scoped credential use for basic-auth git secrets.
		// An annotations read failure must fail closed (treat as unannotated)
		// for basic-auth, never skip the check.
		ann, aErr := c.Client.GetSecretAnnotations(ctx, input.GitSource.SecretRef, ns)
		if aErr != nil {
			ann = nil
		}
		if hErr := bundle.CheckGitSecretHost(auth, ann, input.GitSource.RepoURL); hErr != nil {
			errors = append(errors, fmt.Sprintf("input[%d]: %v", i, hErr))
			continue
		}
		resolved[i] = auth
	}
	return resolved, errors
}

// resolveComposedBundleOCIAuth reads the Secret referenced by each OCI input
// and returns a map of input index -> OCIAuth, so the composer can pull from
// private registries during dry-run validate/preview. Inputs without a secretRef
// are left absent (public). Errors NEVER contain secret material — only
// structured messages appended to the errors list.
func (c *ConfigCRUD) resolveComposedBundleOCIAuth(ctx context.Context, ns string, spec *v1alpha1.ComposedBundleSpec) (map[int]*bundle.OCIAuth, []string) {
	resolved := make(map[int]*bundle.OCIAuth)
	var errors []string
	for i, input := range spec.Inputs {
		if input.OCISource == nil || input.OCISource.SecretRef == "" {
			continue
		}
		data, err := c.Client.GetSecret(ctx, input.OCISource.SecretRef, ns)
		if err != nil {
			errors = append(errors, fmt.Sprintf("input[%d]: failed to read OCI auth secret %q in namespace %q", i, input.OCISource.SecretRef, ns))
			continue
		}
		auth, err := bundle.OCIAuthFromSecret(data)
		if err != nil {
			errors = append(errors, fmt.Sprintf("input[%d]: bad secret %q: invalid format", i, input.OCISource.SecretRef))
			continue
		}
		resolved[i] = auth
	}
	return resolved, errors
}
