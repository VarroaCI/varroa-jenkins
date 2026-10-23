package controller

import (
	"context"
	"encoding/json"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// tenantBundleContentTimeout bounds one bundles.tenantcontent read. It stays
// below the BFF's configGetTimeout, so the BFF does not report the member
// as unreachable while this operator still reads.
const tenantBundleContentTimeout = 2 * time.Second

// tenantBundleContentKeys lists the content keys a pipeline read of a
// member-placed Tenant needs. jenkins.yaml and plugins.yaml stay out of the
// reply, so it stays small.
var tenantBundleContentKeys = []string{
	"items.yaml",
	"variables.yaml",
	"pipeline-templates.yaml",
	bundle.DroppedItemKeysConfigMapKey,
}

// HandleBundlesTenantContent handles operator.<cluster>.bundles.tenantcontent.
// It reads the Tenant, its pipelines bundle ref, the ComposedBundle, and the
// bundle's content ConfigMap from this cluster's own store.
func (c *ConfigCRUD) HandleBundlesTenantContent(data []byte) []byte {
	var req bus.TenantBundleContentRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Tenant == "" {
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), tenantBundleContentTimeout)
	defer cancel()

	tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, c.Store, req.Tenant, "")
	if err != nil {
		return c.configReply(bus.TenantBundleContentResponse{Code: k8sConfigErrToCode(err), Error: err.Error()})
	}
	// The hold is read on every reply, so Held always reports the state of
	// every member cell for the Tenant, whether or not a bundle or its content exists.
	held, err := TenantHeld(ctx, c.Store, tenant)
	if err != nil {
		c.Logger.Error("get tenant cell failed", "tenant", req.Tenant, "error", err)
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}
	if tenant.Spec.Pipelines == nil || tenant.Spec.Pipelines.ComposedBundleRef == nil {
		return c.configReply(bus.TenantBundleContentResponse{Held: held})
	}
	ref := tenant.Spec.Pipelines.ComposedBundleRef

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, c.Store, ref.Name, ref.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.TenantBundleContentResponse{Held: held})
		}
		c.Logger.Error("get tenant bundle failed", "tenant", req.Tenant, "error", err)
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}
	raw, err := marshalCR(cb)
	if err != nil {
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if cb.Status.ContentRef == "" {
		return c.configReply(bus.TenantBundleContentResponse{Bundle: raw, Held: held})
	}

	cm, err := c.Client.GetConfigMap(ctx, cb.Status.ContentRef, ref.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.TenantBundleContentResponse{Bundle: raw, Held: held})
		}
		c.Logger.Error("get tenant bundle content failed", "tenant", req.Tenant, "error", err)
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	content := make(map[string]string, len(tenantBundleContentKeys))
	for _, key := range tenantBundleContentKeys {
		if v, ok := cm[key]; ok {
			content[key] = v
		}
	}
	reply := bus.TenantBundleContentResponse{Bundle: raw, Content: content, Held: held}
	out, err := json.Marshal(reply)
	if err != nil {
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if len(out) > bus.ListPageBudget {
		return c.configReply(bus.TenantBundleContentResponse{Code: bus.CodeInternal, Error: "tenant bundle content exceeds the reply budget"})
	}
	return out
}
