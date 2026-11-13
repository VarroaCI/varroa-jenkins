//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// =============================================================================
// ProvisioningDefaults tools (2)
// =============================================================================

func registerProvisioningDefaultsTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	clusterOrDefault := func(args map[string]any) string {
		if c := strArg(args, "cluster"); c != "" {
			return c
		}
		if deps.Brood != nil && deps.Brood.LocalCluster() != "" {
			return deps.Brood.LocalCluster()
		}
		return "core"
	}

	getPD := mcp.NewTool("get_provisioning_defaults",
		mcp.WithDescription("Get a provisioning defaults configuration by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Provisioning defaults name (e.g. 'varroa-defaults')")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindRead, getPD, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		args := getArgs(req)
		name, cluster := strArg(args, "name"), clusterOrDefault(args)
		raw, err := deps.ConfigBrood.GetProvisioningDefaults(ctx, cluster, name)
		if err != nil {
			return mcp.NewToolResultError("provisioning defaults not found: " + err.Error()), nil
		}
		return resultJSON(raw)
	})

	updatePD := mcp.NewTool("update_provisioning_defaults",
		mcp.WithDescription("Update provisioning defaults configuration. The tool sets only the fields you name. When another writer creates or changes the object during the call, the tool reads it again and merges again, up to three times."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Provisioning defaults name")),
		mcp.WithString("rootDomain", mcp.Description("Root domain for auto-derived ingress hosts")),
		mcp.WithString("storageClass", mcp.Description("Default storage class")),
		mcp.WithString("defaultStorage", mcp.Description("Default PVC size (e.g. 20Gi)")),
		mcp.WithString("defaultVersion", mcp.Description("Default Jenkins version")),
		mcp.WithString("defaultCPU", mcp.Description("Default CPU request")),
		mcp.WithString("defaultMemory", mcp.Description("Default memory request")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindUpdate, updatePD, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanUpdateProvisioningDefaults(claims) {
			return mcp.NewToolResultError("access denied: missing provisioningdefaults:update permission"), nil
		}
		args := getArgs(req)
		name, cluster := strArg(args, "name"), clusterOrDefault(args)
		var out json.RawMessage
		for attempt := 1; ; attempt++ {
			// Each round reads the target again, so a conflict merges onto the newer object.
			existing, err := readProvisioningDefaultsBase(ctx, deps, cluster, name)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to read provisioning defaults: %v", err)), nil
			}
			mergeProvisioningDefaultsArgs(&existing.Spec, args)
			out, err = writeProvisioningDefaults(ctx, deps, cluster, name, existing)
			if err == nil {
				break
			}
			if !isConflictErr(err) {
				return mcp.NewToolResultError(fmt.Sprintf("failed to update provisioning defaults: %v", err)), nil
			}
			if attempt == updateWriteAttempts {
				return mcp.NewToolResultError(fmt.Sprintf("provisioning defaults %s on cluster %s changed during each of %d attempts: call update_provisioning_defaults again", name, cluster, updateWriteAttempts)), nil
			}
		}
		emitActivityForCluster(ctx, deps, claims, cluster, activity.Event{
			Type:    "provisioningdefaults.updated",
			Message: "ProvisioningDefaults " + name + " updated",
		})
		return resultJSON(out)
	})

	listJVP := mcp.NewTool("list_jenkins_version_profile",
		mcp.WithDescription("List JenkinsVersionProfile CRDs on a cluster. Returns a compact "+
			"summary per profile (name, version, channel, pluginSetRef, pluginSetReady, "+
			"notReadyReason) \u2014 enough to survey the version catalog and spot profiles "+
			"whose plugin set has not materialized. Use get_jenkins_version_profile for the "+
			"full resource."),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
		mcp.WithBoolean("verbose", mcp.Description("Return full JenkinsVersionProfile resources "+
			"instead of summaries. Prefer get_jenkins_version_profile for detail on a "+
			"specific profile.")),
		withListOutput(versionProfileListOutputSchema),
	)
	addTool(mcpServer, kindRead, listJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		cluster := clusterOrDefault(args)
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		items, err := deps.ConfigBrood.ListVersionProfiles(ctx, cluster)
		if err != nil {
			return mcp.NewToolResultError("failed to list version profiles: " + err.Error()), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(items)
		}
		// ListVersionProfiles returns raw JSON, not typed CRs. Each element must
		// unmarshal before it can be projected; an element that is not a
		// JenkinsVersionProfile is skipped rather than surfacing as a broken
		// summary. A null element unmarshals without error into the zero struct,
		// so the empty name is the guard against emitting a nameless summary.
		summaries := make([]versionProfileSummary, 0, len(items))
		for _, raw := range items {
			var p v1alpha1.JenkinsVersionProfile
			if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
				continue
			}
			summaries = append(summaries, summarizeVersionProfile(&p))
		}
		return resultJSON(summaries)
	})

	getJVP := mcp.NewTool("get_jenkins_version_profile",
		mcp.WithDescription("Get a JenkinsVersionProfile CRD"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Profile name")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindRead, getJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		args := getArgs(req)
		raw, err := deps.ConfigBrood.GetVersionProfile(ctx, clusterOrDefault(args), strArg(args, "name"))
		if err != nil {
			return mcp.NewToolResultError("version profile not found: " + err.Error()), nil
		}
		return resultJSON(raw)
	})

	writeProfile := func(verb string, authz func(*auth.Claims) bool, call func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if deps.ConfigBrood == nil {
				return mcp.NewToolResultError("config brood not configured"), nil
			}
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if deps.Authorizer == nil || !authz(claims) {
				return mcp.NewToolResultError("access denied: missing version-profiles:" + verb + " permission"), nil
			}
			args := getArgs(req)
			name := strArg(args, "name")
			var obj map[string]any
			if raw := strArg(args, "object"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &obj); err != nil {
					return mcp.NewToolResultError("invalid object JSON: " + err.Error()), nil
				}
			} else {
				obj = map[string]any{"apiVersion": "varroa.dev/v1alpha1", "kind": "JenkinsVersionProfile", "metadata": map[string]any{"name": name}, "spec": map[string]any{}}
			}
			raw, _ := json.Marshal(obj)
			cluster := clusterOrDefault(args)
			out, err := call(ctx, cluster, name, raw)
			if err != nil {
				return mcp.NewToolResultError("failed to " + verb + " version profile: " + err.Error()), nil
			}
			emitActivityForCluster(ctx, deps, claims, cluster, activity.Event{
				Type:    "versionprofile." + verb + "d",
				Message: "JenkinsVersionProfile " + name + " " + verb + "d",
			})
			return resultJSON(out)
		}
	}

	for _, spec := range []struct {
		name string
		kind toolKind
		desc string
		auth func(*auth.Claims) bool
		call func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	}{
		{
			"create_jenkins_version_profile",
			// Reaches the operator's crdstore.Create with no Apply fallback, so
			// unlike every other create here it cannot overwrite.
			kindCreateStrict,
			"Create a JenkinsVersionProfile CRD",
			func(claims *auth.Claims) bool {
				return deps.Authorizer != nil && deps.Authorizer.CanCreateVersionProfile(claims)
			},
			func(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
				if deps.ConfigBrood == nil {
					return nil, fmt.Errorf("config brood not configured")
				}
				return deps.ConfigBrood.CreateVersionProfile(ctx, cluster, name, obj)
			},
		},
	} {
		tool := mcp.NewTool(spec.name, mcp.WithDescription(spec.desc), mcp.WithString("name", mcp.Required()), mcp.WithString("object"), mcp.WithString("cluster"))
		addTool(mcpServer, spec.kind, tool, writeProfile(strings.TrimSuffix(spec.name, "_jenkins_version_profile"), spec.auth, spec.call))
	}

	updateJVP := mcp.NewTool("update_jenkins_version_profile",
		mcp.WithDescription("Update a JenkinsVersionProfile CRD. The tool merges object onto the stored profile as a JSON merge patch: a key you name replaces the stored value, null removes it, and a key you leave out keeps the stored value. The tool ignores metadata.resourceVersion in object. When another writer changes the profile during the call, the tool reads it again and merges again, up to three times."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Profile name")),
		mcp.WithString("object", mcp.Description("A JSON merge patch for the profile, for example {\"spec\":{\"recommended\":true}}")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindUpdate, updateJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanUpdateVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:update permission"), nil
		}
		args := getArgs(req)
		name, cluster := strArg(args, "name"), clusterOrDefault(args)
		patch := []byte("{}")
		noChange := true
		if raw := strArg(args, "object"); raw != "" {
			var probe map[string]any
			if err := json.Unmarshal([]byte(raw), &probe); err != nil {
				return mcp.NewToolResultError("invalid object JSON: " + err.Error()), nil
			}
			if probe == nil {
				return mcp.NewToolResultError("invalid object JSON: the patch must be a JSON object"), nil
			}
			patch = []byte(raw)
			noChange = len(probe) == 0
		}
		if noChange {
			// An empty patch changes nothing. Answer the stored profile and write nothing.
			base, err := deps.ConfigBrood.GetVersionProfile(ctx, cluster, name)
			if err != nil {
				return mcp.NewToolResultError(versionProfileReadError(err)), nil
			}
			return resultJSON(base)
		}
		var out json.RawMessage
		for attempt := 1; ; attempt++ {
			// Each round reads the profile again, so a conflict merges onto the newer object.
			base, err := deps.ConfigBrood.GetVersionProfile(ctx, cluster, name)
			if err != nil {
				return mcp.NewToolResultError(versionProfileReadError(err)), nil
			}
			obj, err := mergeVersionProfileObject(base, patch, name)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			out, err = deps.ConfigBrood.UpdateVersionProfile(ctx, cluster, name, obj)
			if err == nil {
				break
			}
			if !isConflictErr(err) {
				return mcp.NewToolResultError("failed to update version profile: " + err.Error()), nil
			}
			if attempt == updateWriteAttempts {
				return mcp.NewToolResultError(fmt.Sprintf("version profile %s on cluster %s changed during each of %d attempts: call update_jenkins_version_profile again", name, cluster, updateWriteAttempts)), nil
			}
		}
		emitActivityForCluster(ctx, deps, claims, cluster, activity.Event{
			Type:    "versionprofile.updated",
			Message: "JenkinsVersionProfile " + name + " updated",
		})
		return resultJSON(out)
	})

	deleteJVP := mcp.NewTool("delete_jenkins_version_profile", mcp.WithDescription("Delete a JenkinsVersionProfile CRD"), mcp.WithString("name", mcp.Required()), mcp.WithString("cluster"))
	addTool(mcpServer, kindDelete, deleteJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanDeleteVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:delete permission"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		cluster := clusterOrDefault(args)
		if err := deps.ConfigBrood.DeleteVersionProfile(ctx, cluster, name); err != nil {
			return mcp.NewToolResultError("failed to delete version profile: " + err.Error()), nil
		}
		emitActivityForCluster(ctx, deps, claims, cluster, activity.Event{
			Type:    "versionprofile.deleted",
			Message: "JenkinsVersionProfile " + name + " deleted",
		})
		return mcp.NewToolResultText("version profile " + name + " deleted"), nil
	})
}

// mergeProvisioningDefaultsArgs sets each spec field that the caller named.
// A field the caller leaves out keeps the stored value.
func mergeProvisioningDefaultsArgs(spec *v1alpha1.ProvisioningDefaultsSpec, args map[string]any) {
	if v := strArg(args, "rootDomain"); v != "" {
		spec.RootDomain = v
	}
	if v := strArg(args, "storageClass"); v != "" {
		spec.StorageClass = v
	}
	if v := strArg(args, "defaultStorage"); v != "" {
		spec.DefaultStorage = v
	}
	if v := strArg(args, "defaultVersion"); v != "" {
		spec.DefaultVersion = v
	}
	if v := strArg(args, "defaultCPU"); v != "" {
		spec.DefaultCPU = v
	}
	if v := strArg(args, "defaultMemory"); v != "" {
		spec.DefaultMemory = v
	}
}

// writeProvisioningDefaults sends pd to the operator of the target cluster,
// also on the local cluster. The write carries pd's resourceVersion.
func writeProvisioningDefaults(ctx context.Context, deps *api.Dependencies, cluster, name string, pd *v1alpha1.ProvisioningDefaults) (json.RawMessage, error) {
	raw, err := json.Marshal(pd)
	if err != nil {
		return nil, err
	}
	return deps.ConfigBrood.UpdateProvisioningDefaults(ctx, cluster, name, raw)
}

// readProvisioningDefaultsBase returns the stored spec that a patch merges onto.
// The base comes from the target cluster, so a merge never copies another
// cluster's spec. A not-found answer yields an empty object. Any other read
// error fails the call, so a transient error cannot wipe the target's spec.
// The empty object carries no resourceVersion, so its write only creates the
// object. A create by another writer answers conflict, and the caller reads again.
func readProvisioningDefaultsBase(ctx context.Context, deps *api.Dependencies, cluster, name string) (*v1alpha1.ProvisioningDefaults, error) {
	empty := &v1alpha1.ProvisioningDefaults{ObjectMeta: metav1.ObjectMeta{Name: name}}
	raw, err := deps.ConfigBrood.GetProvisioningDefaults(ctx, cluster, name)
	if isNotFoundErr(err) {
		return empty, nil
	}
	if err != nil {
		return nil, err
	}
	var pd v1alpha1.ProvisioningDefaults
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("decode provisioning defaults: %w", err)
	}
	return &pd, nil
}

// isNotFoundErr reports a not-found answer from the local store or from a
// member cluster over the bus.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var be *api.BroodError
	return apierrors.IsNotFound(err) || (errors.As(err, &be) && be.Code == bus.CodeNotFound)
}

// versionProfileReadError words a failed read of a version profile. Only a
// not-found answer says the profile is missing. Any other failure says the read failed.
func versionProfileReadError(err error) string {
	if isNotFoundErr(err) {
		return "version profile not found: " + err.Error()
	}
	return "failed to read version profile: " + err.Error()
}

// mergeVersionProfileObject applies patch to base as a JSON merge patch. The
// result keeps the name and the resourceVersion of base, so the write fences
// on the read whatever patch holds.
func mergeVersionProfileObject(base json.RawMessage, patch []byte, name string) (json.RawMessage, error) {
	var read struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(base, &read); err != nil {
		return nil, fmt.Errorf("decode version profile: %w", err)
	}
	merged, err := jsonpatch.MergePatch(base, patch)
	if err != nil {
		return nil, fmt.Errorf("invalid object JSON: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(merged, &obj); err != nil {
		return nil, fmt.Errorf("decode merged version profile: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("invalid object JSON: the patch must be a JSON object")
	}
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		md = map[string]any{}
		obj["metadata"] = md
	}
	md["name"] = name
	md["resourceVersion"] = read.Metadata.ResourceVersion
	obj["apiVersion"] = "varroa.dev/v1alpha1"
	obj["kind"] = "JenkinsVersionProfile"
	return json.Marshal(obj)
}

// versionProfileSummary is the default projection for
// list_jenkins_version_profile.
//
// The cost at fleet scale is status.conditions[] — two full condition objects
// per profile, each carrying lastTransitionTime, reason and message. They
// collapse to a single ready boolean plus the reason. JenkinsVersionProfile is
// cluster-scoped (types.go: +kubebuilder:resource:scope=Cluster), so the
// summary carries no namespace. PluginSetRef is a *ConfigMapRef that is nil on
// metadata-only profiles; only the referenced ConfigMap's name is projected,
// and only when the ref exists.
type versionProfileSummary struct {
	Name           string `json:"name"`
	Version        string `json:"version,omitempty"`
	Channel        string `json:"channel,omitempty"`
	PluginSetRef   string `json:"pluginSetRef,omitempty"`
	PluginSetReady bool   `json:"pluginSetReady"`
	NotReadyReason string `json:"notReadyReason,omitempty"`
}

func summarizeVersionProfile(p *v1alpha1.JenkinsVersionProfile) versionProfileSummary {
	if p == nil {
		return versionProfileSummary{}
	}
	s := versionProfileSummary{
		Name:    p.Name,
		Version: p.Spec.Version,
		Channel: p.Spec.Channel,
	}
	if p.Spec.PluginSetRef != nil {
		s.PluginSetRef = p.Spec.PluginSetRef.Name
	}
	for _, c := range p.Status.Conditions {
		if c.Type != "PluginSetReady" {
			continue
		}
		if c.Status == metav1.ConditionTrue {
			s.PluginSetReady = true
		} else {
			s.NotReadyReason = c.Reason
		}
		break
	}
	return s
}

var versionProfileListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "JenkinsVersionProfile name."},
	{Name: "version", Type: "string", Desc: "Pinned Jenkins version or LTS line."},
	{Name: "channel", Type: "string", Desc: "Release channel (lts or weekly)."},
	{Name: "pluginSetRef", Type: "string", Desc: "ConfigMap holding the resolved plugin set."},
	{Name: "pluginSetReady", Type: "boolean", Desc: "Whether the pinned plugin set is materialized and ready."},
	{Name: "notReadyReason", Type: "string", Desc: "Reason the plugin set is not ready, when it is not."},
})
