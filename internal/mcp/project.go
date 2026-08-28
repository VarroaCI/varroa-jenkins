package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// controllerSummary is what list_controllers returns by default.
//
// A full Controller CR is roughly 100x this size, dominated by
// metadata.managedFields and a status block with plugin inventories and apply
// history. None of that helps a caller surveying a fleet: the questions are
// "what exists, where, is it healthy, what version". Anything a caller
// needs beyond it is one get_controller away, which is the trade: the result
// now scales with the number of controllers rather than with CR verbosity.
type controllerSummary struct {
	Name               string `json:"name"`
	Namespace          string `json:"namespace"`
	Cluster            string `json:"cluster,omitempty"`
	Phase              string `json:"phase,omitempty"`
	Version            string `json:"version,omitempty"`
	PowerState         string `json:"powerState,omitempty"`
	CreatedAt          string `json:"createdAt,omitempty"`
	LastReconcileError string `json:"lastReconcileError,omitempty"`
}

// summarizeController projects a Controller CR down to its summary.
//
// cluster is a parameter rather than a field read: cluster identity lives on
// api.ClusterController, the brood wrapper, and never on the CR itself. The
// local-fallback path has no wrapper, so it passes the local cluster name.
func summarizeController(cluster string, cr *v1alpha1.Controller) controllerSummary {
	if cr == nil {
		return controllerSummary{Cluster: cluster}
	}
	created := ""
	if !cr.CreationTimestamp.IsZero() {
		created = cr.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")
	}
	return controllerSummary{
		Name:               cr.Name,
		Namespace:          cr.Namespace,
		Cluster:            cluster,
		Phase:              string(cr.Status.Phase),
		Version:            cr.Spec.Version,
		PowerState:         cr.Spec.PowerState,
		CreatedAt:          created,
		LastReconcileError: cr.Status.LastReconcileError,
	}
}

// schemaField describes one property of a summary projection. Type is a JSON
// Schema primitive: "string", "integer", or "boolean".
type schemaField struct {
	Name string
	Type string
	Desc string
}

// listOutputSchema builds the dual-shape output schema shared by every
// projected list_* tool: a boxed {items, count} object whose members are
// either a summary or, under verbose, a full resource.
//
// The item schema must validate both shapes, so the branches are joined with
// anyOf. oneOf is wrong here — a summary object also satisfies the open
// {"type":"object"} verbose branch, and oneOf requires exactly one match, so it
// would reject every default result. Emitting structuredContent that violates
// the tool's own declared schema is the class of bug PR #466 fixed; declaring a
// schema that cannot accept our own output would reintroduce it.
//
// Note that the server does not enforce this: handler.go enables input schema
// validation only. It is a contract for clients, asserted in tests here.
// Every interpolated string goes through jsonString, never %q: Go quoting and
// JSON quoting are not the same language. %q emits \a, \v and \xNN, none of
// which are valid JSON escapes, so a description containing a control character
// would silently produce an unparseable schema.
func listOutputSchema(summaryDesc string, fields []schemaField) json.RawMessage {
	props := make([]string, 0, len(fields))
	for _, f := range fields {
		props = append(props, fmt.Sprintf(
			`              %s: {"type": %s, "description": %s}`,
			jsonString(f.Name), jsonString(f.Type), jsonString(f.Desc)))
	}
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "required": ["items", "count"],
  "properties": {
    "count": {"type": "integer", "description": "Number of items returned."},
    "items": {
      "type": "array",
      "items": {
        "anyOf": [
          {
            "type": "object",
            "description": %s,
            "properties": {
%s
            }
          },
          {
            "type": "object",
            "description": "Full resource, returned when verbose is true."
          }
        ]
      }
    }
  }
}`, jsonString(summaryDesc), strings.Join(props, ",\n")))
}

// jsonString renders s as a JSON string literal, quotes included.
//
// json.Marshal cannot fail for a string, so the error is discarded rather than
// propagated through every caller of listOutputSchema.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // unreachable for string input
		return `""`
	}
	return string(b)
}

// withListOutput attaches a schema built by listOutputSchema to a tool. It
// exists so domain files read consistently and the attach mechanism has one
// place to change.
func withListOutput(schema json.RawMessage) mcp.ToolOption {
	return mcp.WithRawOutputSchema(schema)
}

// controllerListOutputSchema describes the result of list_controllers.
var controllerListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "Controller name."},
	{Name: "namespace", Type: "string", Desc: "Controller namespace."},
	{Name: "cluster", Type: "string", Desc: "Cluster the controller lives on."},
	{Name: "phase", Type: "string", Desc: "Lifecycle phase."},
	{Name: "version", Type: "string", Desc: "Pinned Jenkins version."},
	{Name: "powerState", Type: "string", Desc: "Running or hibernated."},
	{Name: "createdAt", Type: "string", Desc: "Creation timestamp, RFC3339 UTC."},
	{Name: "lastReconcileError", Type: "string", Desc: "Most recent reconcile error, if any."},
})
