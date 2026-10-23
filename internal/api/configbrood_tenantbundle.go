package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// GetTenantBundleContent reads a member-placed Tenant's ComposedBundle and the
// content keys of its bundle, over the member's bundles.tenantcontent verb.
// The member answers a nil bundle when the Tenant has no bundle ref, and a
// nil map when the bundle has no content.
func (f *busConfigBrood) GetTenantBundleContent(ctx context.Context, cluster, tenant string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	if f.isLocal(cluster) {
		return nil, nil, false, &BroodError{Code: bus.CodeInternal, Msg: "the local cluster's tenant bundles are read on the local store"}
	}
	respData, err := f.readTo(ctx, bus.OperatorBundlesSubject(cluster, "tenantcontent"), bus.TenantBundleContentRequest{Tenant: tenant}, configGetTimeout)
	if err != nil {
		return nil, nil, false, err
	}
	var resp bus.TenantBundleContentResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, nil, false, fmt.Errorf("tenantcontent decode: %w", err)
	}
	if resp.Code != "" {
		return nil, nil, false, f.codeToErr(resp.Code, resp.Error)
	}
	var cb *v1alpha1.ComposedBundle
	if len(resp.Bundle) > 0 {
		cb = &v1alpha1.ComposedBundle{}
		if err := json.Unmarshal(resp.Bundle, cb); err != nil {
			return nil, nil, false, fmt.Errorf("tenantcontent decode bundle: %w", err)
		}
	}
	return cb, resp.Content, resp.Held, nil
}
