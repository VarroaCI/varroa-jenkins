package mcp

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// clusterArgDescription is the schema text of the cluster argument of a config tool.
const clusterArgDescription = "Target cluster (default: local cluster). With a cluster, the tool reads or writes the object on that cluster only."

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
// argument or the local name selects the local cluster, which the tool reads and
// writes on deps.Store. Any other name must be a member that deps.Brood knows.
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
	if c.local {
		return storeObject[v1alpha1.ComposedBundle](deps.Store, name, namespace)
	}
	return broodObject[v1alpha1.ComposedBundle](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetComposedBundle(ctx, c.name, namespace, name)
		},
		func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.UpdateComposedBundle(ctx, c.name, namespace, name, obj)
		},
	)
}

// catalogSourceObject addresses one CatalogSource on the cluster c.
func catalogSourceObject(deps *api.Dependencies, c configCluster, namespace, name string) configObject[v1alpha1.CatalogSource] {
	if c.local {
		return storeObject[v1alpha1.CatalogSource](deps.Store, name, namespace)
	}
	return broodObject[v1alpha1.CatalogSource](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetCatalogSource(ctx, c.name, namespace, name)
		},
		func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.UpdateCatalogSource(ctx, c.name, namespace, name, obj)
		},
	)
}

// jenkinsRoleObject addresses one cluster-scoped JenkinsRole on the cluster c.
func jenkinsRoleObject(deps *api.Dependencies, c configCluster, name string) configObject[v1alpha1.JenkinsRole] {
	if c.local {
		return storeObject[v1alpha1.JenkinsRole](deps.Store, name, "")
	}
	return broodObject[v1alpha1.JenkinsRole](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetJenkinsRole(ctx, c.name, name)
		},
		func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.UpdateJenkinsRole(ctx, c.name, name, obj)
		},
	)
}

// jenkinsRoleBindingObject addresses one cluster-scoped JenkinsRoleBinding on the cluster c.
func jenkinsRoleBindingObject(deps *api.Dependencies, c configCluster, name string) configObject[v1alpha1.JenkinsRoleBinding] {
	if c.local {
		return storeObject[v1alpha1.JenkinsRoleBinding](deps.Store, name, "")
	}
	return broodObject[v1alpha1.JenkinsRoleBinding](
		func(ctx context.Context) (json.RawMessage, error) {
			return deps.ConfigBrood.GetJenkinsRoleBinding(ctx, c.name, name)
		},
		func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.UpdateJenkinsRoleBinding(ctx, c.name, name, obj)
		},
	)
}
