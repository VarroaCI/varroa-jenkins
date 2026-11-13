package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// clusterArgDescription is the schema text of the cluster argument of a config tool.
const clusterArgDescription = "Target cluster (default: local cluster). With a cluster, the tool acts on that cluster only."

// configCluster is the one cluster that a config tool call reads or writes.
type configCluster struct {
	name  string
	local bool
}

// onCluster names a member cluster in a tool error. It is empty for the local cluster.
func (c configCluster) onCluster() string {
	if c.local {
		return ""
	}
	return " on cluster " + c.name
}

// operatorNamespace returns the operator namespace of the cluster for the read gate.
func (c configCluster) operatorNamespace(ctx context.Context, deps *api.Dependencies) string {
	if c.local {
		return deps.OperatorNamespace
	}
	return api.NewServer(deps).ConfigOperatorNamespace(ctx, c.name)
}

// resolveConfigCluster maps the cluster argument onto one cluster. An empty
// argument or the local name selects the local cluster, which a read tool reads
// on deps.Store. Any other name must be a member that deps.Brood knows.
// Without a brood, only the local cluster is known.
func resolveConfigCluster(ctx context.Context, deps *api.Dependencies, args map[string]any) (configCluster, *mcp.CallToolResult) {
	local := localCluster(deps)
	name := strArg(args, "cluster")
	if name == "" || name == local {
		return configCluster{name: local, local: true}, nil
	}
	if deps.Brood == nil || !deps.Brood.IsKnown(ctx, name) {
		return configCluster{}, mcp.NewToolResultError("unknown cluster: " + name)
	}
	if deps.ConfigBrood == nil {
		return configCluster{}, mcp.NewToolResultError("config brood not configured")
	}
	return configCluster{name: name}, nil
}

// resolveConfigWriteCluster is resolveConfigCluster for a create, an update, or
// a delete. The operator of the target cluster runs every write, also on the
// local cluster, so a server without deps.ConfigBrood refuses the call.
func resolveConfigWriteCluster(ctx context.Context, deps *api.Dependencies, args map[string]any) (configCluster, *mcp.CallToolResult) {
	c, res := resolveConfigCluster(ctx, deps, args)
	if res != nil {
		return configCluster{}, res
	}
	if deps.ConfigBrood == nil {
		return configCluster{}, mcp.NewToolResultError("config brood not configured")
	}
	return c, nil
}

// emitConfigActivity stamps a member cluster on e. A local write keeps the publisher's own cluster.
func emitConfigActivity(ctx context.Context, deps *api.Dependencies, claims *auth.Claims, c configCluster, e activity.Event) {
	if c.local {
		emitActivity(ctx, deps, claims, e)
		return
	}
	emitActivityForCluster(ctx, deps, claims, c.name, e)
}

// composedBundleObject addresses one ComposedBundle on the cluster c.
func composedBundleObject(deps *api.Dependencies, c configCluster, namespace, name string) configObject[v1alpha1.ComposedBundle] {
	update := func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
		return deps.ConfigBrood.UpdateComposedBundle(ctx, c.name, namespace, name, obj)
	}
	if c.local {
		return localObject[v1alpha1.ComposedBundle](deps.Store, name, namespace, update)
	}
	return broodObject[v1alpha1.ComposedBundle](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetComposedBundle(ctx, c.name, namespace, name)
		},
		update,
	)
}

// catalogSourceObject addresses one CatalogSource on the cluster c.
func catalogSourceObject(deps *api.Dependencies, c configCluster, namespace, name string) configObject[v1alpha1.CatalogSource] {
	update := func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
		return deps.ConfigBrood.UpdateCatalogSource(ctx, c.name, namespace, name, obj)
	}
	if c.local {
		return localObject[v1alpha1.CatalogSource](deps.Store, name, namespace, update)
	}
	return broodObject[v1alpha1.CatalogSource](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetCatalogSource(ctx, c.name, namespace, name)
		},
		update,
	)
}

// jenkinsRoleObject addresses one cluster-scoped JenkinsRole on the cluster c.
func jenkinsRoleObject(deps *api.Dependencies, c configCluster, name string) configObject[v1alpha1.JenkinsRole] {
	update := func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
		return deps.ConfigBrood.UpdateJenkinsRole(ctx, c.name, name, obj)
	}
	if c.local {
		return localObject[v1alpha1.JenkinsRole](deps.Store, name, "", update)
	}
	return broodObject[v1alpha1.JenkinsRole](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetJenkinsRole(ctx, c.name, name)
		},
		update,
	)
}

// jenkinsRoleBindingObject addresses one cluster-scoped JenkinsRoleBinding on the cluster c.
func jenkinsRoleBindingObject(deps *api.Dependencies, c configCluster, name string) configObject[v1alpha1.JenkinsRoleBinding] {
	update := func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
		return deps.ConfigBrood.UpdateJenkinsRoleBinding(ctx, c.name, name, obj)
	}
	if c.local {
		return localObject[v1alpha1.JenkinsRoleBinding](deps.Store, name, "", update)
	}
	return broodObject[v1alpha1.JenkinsRoleBinding](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetJenkinsRoleBinding(ctx, c.name, name)
		},
		update,
	)
}

// listOnCluster lists the objects of one kind on the cluster c. The local
// cluster reads deps.Store. A member answers through list, one JSON object for
// each item.
func listOnCluster[T any](ctx context.Context, deps *api.Dependencies, c configCluster, namespace string,
	list func(ctx context.Context) ([]json.RawMessage, error)) ([]*T, error) {
	if c.local {
		return crdstore.List[T](ctx, deps.Store, namespace, "")
	}
	raws, err := list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(raws))
	for i, raw := range raws {
		var obj T
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("decode item %d: %w", i, err)
		}
		out = append(out, &obj)
	}
	return out, nil
}

// createOnCluster sends obj through create, the ConfigBrood create method of
// its kind, and returns the stored object. The operator of the target cluster
// runs the create, also on the local cluster. A refusal of an existing name is
// a conflict answer.
func createOnCluster[T any](ctx context.Context, obj *T,
	create func(ctx context.Context, body json.RawMessage) (json.RawMessage, error)) (*T, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode the object: %w", err)
	}
	raw, err := create(ctx, body)
	if err != nil {
		return nil, err
	}
	var created T
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("decode the created object: %w", err)
	}
	return &created, nil
}

// isAlreadyExistsErr reports a create refusal for an existing name. The
// operator of the target cluster answers it as conflict.
func isAlreadyExistsErr(err error) bool {
	var be *api.BroodError
	return errors.As(err, &be) && be.Code == bus.CodeConflict
}

// alreadyExistsText words a create refusal. On a member, the text names the
// cluster and the update call repeats it.
func alreadyExistsText(object, updateTool string, c configCluster) string {
	if c.local {
		return object + " already exists; use " + updateTool + " to change it"
	}
	return object + " on cluster " + c.name + " already exists; use " + updateTool + " with cluster " + c.name + " to change it"
}

// deletedResult reports a deleted object. The text names the object and, on a
// member, the cluster. It never carries the object body.
func deletedResult(object string, c configCluster) *mcp.CallToolResult {
	return mcp.NewToolResultText(object + c.onCluster() + " deleted")
}

// catalogItemObject addresses one CatalogItem on the cluster c. A catalog item
// is read-only, so the object has no write.
func catalogItemObject(deps *api.Dependencies, c configCluster, namespace, name string) configObject[v1alpha1.CatalogItem] {
	if c.local {
		return storeObject[v1alpha1.CatalogItem](deps.Store, name, namespace)
	}
	return broodObject[v1alpha1.CatalogItem](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetCatalogItem(ctx, c.name, namespace, name)
		},
		nil,
	)
}
