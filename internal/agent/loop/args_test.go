package loop

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

const strictSchema = `{"additionalProperties":false,"properties":{"run":{"type":"string"},"stage":{"type":"string"}},"required":["run","stage"],"type":"object"}`

func TestCheckArguments(t *testing.T) {
	const prefix = "input schema validation failed: "
	def := agent.ToolDef{Name: "restart_run", Schema: json.RawMessage(strictSchema)}
	tests := []struct {
		name string
		args string
		want string
		ok   bool
	}{
		{"valid object", `{"run":"r","stage":"s"}`, "", true},
		{"undeclared argument", `{"run":"r","stage":"s","scope":1}`, prefix + "<root>: additional properties 'scope' not allowed", false},
		{"missing argument", `{"run":"r"}`, prefix + "<root>: missing property 'stage'", false},
		{"empty object", `{}`, prefix + "<root>: missing properties 'run', 'stage'", false},
		{"null arguments", `null`, prefix + "<root>: missing properties 'run', 'stage'", false},
		{"empty bytes", ``, prefix + "<root>: missing properties 'run', 'stage'", false},
		{"wrong type", `{"run":5,"stage":"s"}`, prefix + "/run: got number, want string", false},
		{"array instead of object", `[]`, prefix + "<root>: got array, want object", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CheckArguments(def, json.RawMessage(tc.args))
			if ok != tc.ok || got != tc.want {
				t.Errorf("CheckArguments(%s) = %q, %v; want %q, %v", tc.args, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestCheckArgumentsPassesWhenSchemaIsUnusable pins that a missing or broken
// schema lets every call through, as the MCP server does.
func TestCheckArgumentsPassesWhenSchemaIsUnusable(t *testing.T) {
	args := json.RawMessage(`{"scope":1}`)
	for name, schema := range map[string]json.RawMessage{
		"nil":        nil,
		"wrong type": json.RawMessage(`{"type":5}`),
		"not json":   json.RawMessage(`not json`),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := CheckArguments(agent.ToolDef{Name: "x", Schema: schema}, args); !ok || got != "" {
				t.Errorf("CheckArguments = %q, %v; want pass", got, ok)
			}
		})
	}
}
