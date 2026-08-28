//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// resultJSON wraps mcp.NewToolResultJSON with the two invariants the MCP surface
// imposes and mcp-go does not enforce.
//
// First, `structuredContent` MUST be a JSON object. Passing a slice straight
// through emits a top-level array, which strict clients reject outright
// ("expected record, received array"), taking down every list_* tool.
// Collections are therefore boxed as {"items": [...], "count": n}; scalars and
// structs pass through untouched.
//
// Second, no result may carry a credential. Sanitization happens here rather
// than per tool because this is the one chokepoint every tool result already
// passes through: doing it at the call sites would mean 64 places to forget,
// and #467 was exactly that class of omission. Route every tool result through
// this helper rather than mcp.NewToolResultJSON so neither invariant can regress.
func resultJSON(v any) (*mcp.CallToolResult, error) {
	sanitized, err := api.SanitizeObject(v)
	if err != nil {
		// A value that will not marshal cannot be sanitized, and emitting it raw
		// risks leaking whatever it holds. Fail the call instead.
		return mcp.NewToolResultError(fmt.Sprintf("failed to encode result: %v", err)), nil
	}

	// Shape is decided from the ORIGINAL value, not the sanitized one:
	// sanitization round-trips through JSON, which collapses a typed nil slice to
	// null and would lose the "emit [] so clients can iterate unconditionally"
	// guarantee below.
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return mcp.NewToolResultJSON(sanitized)
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return mcp.NewToolResultJSON(sanitized)
	}
	// []byte is a scalar payload, not a collection.
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return mcp.NewToolResultJSON(sanitized)
	}
	items := sanitized
	if items == nil {
		// Emit [] rather than null so clients can iterate unconditionally.
		items = []any{}
	}
	return mcp.NewToolResultJSON(map[string]any{
		"items": items,
		"count": rv.Len(),
	})
}

// broodFailureToolError renders a Brood mutation failure for the MCP surface.
// The operator ships failing preflight checks back alongside a terse error
// ("preflight failed"), and the checks are the only part that says WHICH check
// failed and why — dropping them leaves the caller diagnosing a create/update
// rejection blind. verb names the attempted action for the fallback message.
//
// An SSA field conflict is the same failure in a different envelope: the
// operator answers a bare "field conflict" and puts the actionable half — which
// field, which owning manager — in BroodError.Conflicts. Rendering the error
// alone tells the caller a write was refused but not by whom or how to proceed,
// which is a dead end rather than a diagnosis.
func broodFailureToolError(verb string, err error, checks []bus.Check) *mcp.CallToolResult {
	var be *api.BroodError
	if errors.As(err, &be) && len(be.Conflicts) > 0 {
		b, _ := json.Marshal(be.Conflicts)
		return mcp.NewToolResultError(fmt.Sprintf(
			"failed to %s: %v: %s: those fields are owned by another field manager; "+
				"retry with force=true to take ownership of them",
			verb, err, string(b)))
	}
	var failing []bus.Check
	for _, c := range checks {
		if c.Status == "fail" {
			failing = append(failing, c)
		}
	}
	if len(failing) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("failed to %s: %v", verb, err))
	}
	b, _ := json.Marshal(failing)
	return mcp.NewToolResultError(fmt.Sprintf("failed to %s: %v: %s", verb, err, string(b)))
}

func registerAllTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerControllerTools(mcpServer, deps)
	registerVarroaRoleTools(mcpServer, deps)
	registerVarroaRoleBindingTools(mcpServer, deps)
	registerJenkinsRoleTools(mcpServer, deps)
	registerJenkinsRoleBindingTools(mcpServer, deps)
	registerCatalogSourceTools(mcpServer, deps)
	registerCatalogItemTools(mcpServer, deps)
	registerComposedBundleTools(mcpServer, deps)
	registerProvisioningDefaultsTools(mcpServer, deps)
	registerUserTools(mcpServer, deps)
	registerGroupTools(mcpServer, deps)
	registerActivitySearchMetaTools(mcpServer, deps)
	registerProxyTools(mcpServer, deps)
}

func requireClaims(ctx context.Context) (*auth.Claims, error) {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return nil, fmt.Errorf("authentication required")
	}
	return claims, nil
}

// --- Argument extraction helpers ---

func getArgs(req mcp.CallToolRequest) map[string]any {
	if args, ok := req.Params.Arguments.(map[string]any); ok {
		return args
	}
	return map[string]any{}
}

func namespaceOrDefault(args map[string]any) string {
	ns, _ := args["namespace"].(string)
	return ns
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// mapArg reads an object-valued argument. The second return distinguishes
// "not supplied" from "supplied empty" — callers building merge patches must
// not treat an omitted field as a request to clear it.
func mapArg(args map[string]any, key string) (map[string]any, bool) {
	raw, ok := args[key]
	if !ok {
		return nil, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

func strSliceArg(args map[string]any, key string) ([]string, bool) {
	raw, ok := args[key]
	if !ok {
		return nil, false
	}
	arr, _ := raw.([]interface{})
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

func firstSlice(v []string, _ bool) []string { return v }

func marshalUnmarshal(from, to interface{}) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, to)
}

func objMeta(args map[string]any) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      strArg(args, "name"),
		Namespace: strArg(args, "namespace"),
	}
}

func strSliceAny(v interface{}) []string {
	arr, _ := v.([]interface{})
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func buildSubjectRefs(raw interface{}) []v1alpha1.SubjectRef {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	subjects := make([]v1alpha1.SubjectRef, 0, len(arr))
	for _, s := range arr {
		m, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		subjects = append(subjects, v1alpha1.SubjectRef{
			Kind: strVal(m, "kind"),
			Name: strVal(m, "name"),
		})
	}
	return subjects
}
func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}
