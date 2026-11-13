//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// registerAssistantConfigTools adds the two AssistantConfig tools. Neither tool
// takes a provider key: an assistant session stores its tool arguments in the
// transcript.
func registerAssistantConfigTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	getAC := mcp.NewTool("get_assistant_config",
		mcp.WithDescription("Get the assistant configuration: the providers, the default provider and model, the limits, and the approval expiry. Answers the default spec when the object does not exist. Needs assistant:admin. Provider keys are never returned."),
	)
	addTool(mcpServer, kindRead, getAC, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanAdminAssistant(claims) {
			return mcp.NewToolResultError("access denied: missing assistant:admin permission"), nil
		}
		if deps.Assistant == nil {
			return mcp.NewToolResultError("the assistant is not enabled"), nil
		}
		obj, serr := deps.Assistant.GetConfig(ctx)
		if serr != nil {
			return mcp.NewToolResultError("failed to read the assistant config: " + serr.Message), nil
		}
		return resultJSON(withAssistantTypeMeta(obj))
	})

	updateAC := mcp.NewTool("update_assistant_config",
		mcp.WithDescription("Update the assistant configuration. The object argument is a JSON merge patch over the stored object. The tool keeps the version it read, so a change by another writer answers conflict and the tool merges again, up to three times. Needs assistant:admin. Provider keys are not set by this tool."),
		mcp.WithString("object", mcp.Required(), mcp.Description("JSON merge patch over the AssistantConfig spec, for example {\"spec\":{\"budgets\":{\"session\":300000}}}")),
	)
	addTool(mcpServer, kindUpdate, updateAC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanAdminAssistant(claims) {
			return mcp.NewToolResultError("access denied: missing assistant:admin permission"), nil
		}
		if deps.Assistant == nil {
			return mcp.NewToolResultError("the assistant is not enabled"), nil
		}
		patch := []byte(strArg(getArgs(req), "object"))
		if !json.Valid(patch) {
			return mcp.NewToolResultError("object is not valid JSON"), nil
		}
		var out *v1alpha1.AssistantConfig
		for attempt := 1; ; attempt++ {
			// Each round reads the object again, so a conflict merges onto the newer one.
			base, serr := deps.Assistant.GetConfig(ctx)
			if serr != nil {
				return mcp.NewToolResultError("failed to read the assistant config: " + serr.Message), nil
			}
			merged, err := mergeAssistantConfig(base, patch)
			if err != nil {
				return mcp.NewToolResultError("invalid merge patch: " + err.Error()), nil
			}
			var putErr *api.AssistantConfigError
			out, putErr = deps.Assistant.PutConfig(ctx, merged)
			if putErr == nil {
				break
			}
			if putErr.Status != http.StatusConflict {
				return mcp.NewToolResultError("failed to update the assistant config: " + putErr.Message), nil
			}
			if attempt == updateWriteAttempts {
				return mcp.NewToolResultError(fmt.Sprintf("the assistant config changed during each of %d attempts: call update_assistant_config again", updateWriteAttempts)), nil
			}
		}
		emitActivity(ctx, deps, claims, activity.Event{
			Type:    "assistantconfig.updated",
			Message: "AssistantConfig " + v1alpha1.AssistantConfigName + " updated",
		})
		return resultJSON(withAssistantTypeMeta(out))
	})
}

// withAssistantTypeMeta sets the apiVersion and kind of the singleton on a copy,
// so the answer names its kind even when the store did not set it.
func withAssistantTypeMeta(obj *v1alpha1.AssistantConfig) *v1alpha1.AssistantConfig {
	out := *obj
	out.APIVersion = "varroa.dev/v1alpha1"
	out.Kind = v1alpha1.AssistantConfigKind
	return &out
}

// mergeAssistantConfig applies a JSON merge patch to base and keeps base's
// metadata.resourceVersion, so the write fences on the version that was read.
// The result is named and typed as the singleton.
func mergeAssistantConfig(base *v1alpha1.AssistantConfig, patch []byte) (*v1alpha1.AssistantConfig, error) {
	cur, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	merged, err := jsonpatch.MergePatch(cur, patch)
	if err != nil {
		return nil, err
	}
	var out v1alpha1.AssistantConfig
	if err := json.Unmarshal(merged, &out); err != nil {
		return nil, err
	}
	out.APIVersion = "varroa.dev/v1alpha1"
	out.Kind = v1alpha1.AssistantConfigKind
	out.Name = v1alpha1.AssistantConfigName
	// The patch cannot move the fence: the write keeps the version that was read.
	out.ResourceVersion = base.ResourceVersion
	return &out, nil
}
