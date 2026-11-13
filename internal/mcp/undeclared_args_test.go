package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// undeclaredArg is an argument no tool declares.
const undeclaredArg = "notAnArgument"

// quotedArg is the form in which the schema validator names an argument.
func quotedArg(name string) string { return "'" + name + "'" }

// notAllowedText is the validator text for one undeclared argument.
func notAllowedText(name string) string {
	return "additional properties " + quotedArg(name) + " not allowed"
}

// TestEveryToolSchemaIsStrict pins that each tool publishes a root schema with
// additionalProperties false, and that the schema compiles. The validator skips
// a schema that does not compile, so such a tool would accept any argument.
func TestEveryToolSchemaIsStrict(t *testing.T) {
	for _, tool := range liveTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			var root struct {
				AdditionalProperties *bool `json:"additionalProperties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &root); err != nil {
				t.Fatalf("decode input schema: %v", err)
			}
			if root.AdditionalProperties == nil || *root.AdditionalProperties {
				t.Fatalf("additionalProperties = %v, want false", root.AdditionalProperties)
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.InputSchema))
			if err != nil {
				t.Fatalf("parse input schema: %v", err)
			}
			c := jsonschema.NewCompiler()
			url := "mem:///" + tool.Name + ".json"
			if err := c.AddResource(url, doc); err != nil {
				t.Fatalf("add input schema: %v", err)
			}
			if _, err := c.Compile(url); err != nil {
				t.Fatalf("compile input schema: %v", err)
			}
		})
	}
}

// TestMissingRequiredArgumentIsNamed pins that the refusal of a call without a
// required argument names the argument and differs from the refusal of an
// undeclared argument.
func TestMissingRequiredArgumentIsNamed(t *testing.T) {
	tr := callToolAs(t, adminDeps(), mcpAdminClaims, "get_run", map[string]interface{}{})
	if !tr.IsError {
		t.Fatalf("get_run accepted a call without run: %v", tr.Content)
	}
	text := toolText(tr)
	if !strings.Contains(text, "input schema validation failed: <root>: missing property "+quotedArg("run")) {
		t.Errorf("text = %q, want the documented refusal naming the missing argument run", text)
	}
	if strings.Contains(text, "not allowed") {
		t.Errorf("text = %q, want no undeclared-argument refusal", text)
	}
}

// schemaProp is the subset of a property schema the placeholder builder reads.
type schemaProp struct {
	Type string `json:"type"`
	Enum []any  `json:"enum"`
}

// placeholderArgs fills each required top-level argument of a tool schema with
// a value of its declared type, so the call passes the required check and
// reaches the undeclared-argument refusal.
func placeholderArgs(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var schema struct {
		Properties map[string]schemaProp `json:"properties"`
		Required   []string              `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode input schema: %v", err)
	}
	args := map[string]interface{}{}
	for _, name := range schema.Required {
		prop := schema.Properties[name]
		switch {
		case len(prop.Enum) > 0:
			args[name] = prop.Enum[0]
		case prop.Type == "array":
			args[name] = []interface{}{}
		case prop.Type == "object":
			args[name] = map[string]interface{}{}
		case prop.Type == "number" || prop.Type == "integer":
			args[name] = 1
		case prop.Type == "boolean":
			args[name] = false
		default:
			args[name] = "x"
		}
	}
	return args
}

// TestEveryToolRefusesUndeclaredArgument pins that no tool runs a call that
// carries an argument its schema does not declare.
func TestEveryToolRefusesUndeclaredArgument(t *testing.T) {
	for _, tool := range liveTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			args := placeholderArgs(t, tool.InputSchema)
			args[undeclaredArg] = 1
			tr := callToolAs(t, adminDeps(), mcpAdminClaims, tool.Name, args)
			if !tr.IsError {
				t.Fatalf("%s accepted the undeclared argument: %v", tool.Name, tr.Content)
			}
			want := "input schema validation failed: <root>: " + notAllowedText(undeclaredArg)
			if !strings.Contains(toolText(tr), want) {
				t.Errorf("text = %q, want it to contain %q", toolText(tr), want)
			}
		})
	}
}

// TestEveryMutatingToolRefusesUndeclaredArgument pins that a refused mutating
// call publishes no activity event. TestEveryMutatingToolEmitsActivity proves
// the same arguments without the extra key write and emit one event.
func TestEveryMutatingToolRefusesUndeclaredArgument(t *testing.T) {
	for name, kind := range expectedToolKinds {
		if !kind.mutates() {
			continue
		}
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			deps := guardEmitDeps(t, sink)
			args := guardArgsFor(name)
			args[undeclaredArg] = "x"
			tr := callToolAs(t, deps, guardClaims, name, args)
			if !tr.IsError {
				t.Fatalf("%s accepted the undeclared argument: %v", name, tr.Content)
			}
			if !strings.Contains(toolText(tr), quotedArg(undeclaredArg)) {
				t.Errorf("text = %q, want it to name %q", toolText(tr), undeclaredArg)
			}
			if len(sink.events) != 0 {
				t.Errorf("published %d events for a refused call, want 0", len(sink.events))
			}
		})
	}
}

// TestCreateJenkinsRoleBindingRefusesScopeArgument pins the case from the
// issue: a scope sent to create_jenkins_role_binding must not create a binding
// that applies to every controller.
func TestCreateJenkinsRoleBindingRefusesScopeArgument(t *testing.T) {
	deps := adminDeps()
	args := map[string]interface{}{
		"name":    "b1",
		"roleRef": "viewer",
		"subjects": []interface{}{
			map[string]interface{}{"kind": "User", "name": "alice"},
		},
		"scope": map[string]interface{}{"namespaces": []interface{}{"prod-a"}},
	}
	tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", args)
	if !tr.IsError {
		t.Fatalf("create_jenkins_role_binding accepted scope: %v", tr.Content)
	}
	if !strings.Contains(toolText(tr), notAllowedText("scope")) {
		t.Errorf("text = %q, want it to name scope", toolText(tr))
	}
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), deps.Store, "b1", ""); err == nil {
		t.Error("binding b1 exists after a refused call")
	}
}

// TestInProcessHostRefusesUndeclaredArgument pins the assistant path: the
// in-process host returns the refusal as a tool error and writes nothing.
func TestInProcessHostRefusesUndeclaredArgument(t *testing.T) {
	deps := adminDeps()
	host := NewInProcessHost(NewServer(deps), mcpAdminClaims)
	args := json.RawMessage(`{"name":"b2","roleRef":"viewer","subjects":[{"kind":"User","name":"alice"}],"scope":{"namespaces":["prod-a"]}}`)
	res, err := host.Call(context.Background(), "create_jenkins_role_binding", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Call accepted scope: %q", res.Content)
	}
	if !strings.Contains(res.Content, notAllowedText("scope")) {
		t.Errorf("content = %q, want it to name scope", res.Content)
	}
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), deps.Store, "b2", ""); err == nil {
		t.Error("binding b2 exists after a refused call")
	}
}

// TestJenkinsRBACTools_RefuseMetadataArgs pins that a call that tries to set
// the federation label through a metadata argument is refused and writes
// nothing.
func TestJenkinsRBACTools_RefuseMetadataArgs(t *testing.T) {
	labels := map[string]interface{}{rbac.LabelFederatedFrom: "alpha"}
	subjects := []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}}

	t.Run("create_jenkins_role", func(t *testing.T) {
		deps := adminDeps()
		tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role", map[string]interface{}{
			"name": "r9", "permissions": []interface{}{"Overall/Read"}, "labels": labels,
		})
		if !tr.IsError || !strings.Contains(toolText(tr), notAllowedText("labels")) {
			t.Fatalf("want a refusal naming labels, got %v", tr.Content)
		}
		if _, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), deps.Store, "r9", ""); err == nil {
			t.Error("role r9 exists after a refused call")
		}
	})

	t.Run("create_jenkins_role_binding", func(t *testing.T) {
		deps := adminDeps()
		tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", map[string]interface{}{
			"name": "b9", "roleRef": "viewer", "subjects": subjects, "labels": labels,
		})
		if !tr.IsError || !strings.Contains(toolText(tr), notAllowedText("labels")) {
			t.Fatalf("want a refusal naming labels, got %v", tr.Content)
		}
		if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), deps.Store, "b9", ""); err == nil {
			t.Error("binding b9 exists after a refused call")
		}
	})

	t.Run("update_jenkins_role", func(t *testing.T) {
		deps := adminDeps()
		store := deps.Store.(*crdstore.Fake)
		crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"},
			Spec: v1alpha1.JenkinsRoleSpec{Description: "old"}})
		tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role", map[string]interface{}{
			"name": "r1", "description": "new", "labels": labels,
		})
		if !tr.IsError || !strings.Contains(toolText(tr), notAllowedText("labels")) {
			t.Fatalf("want a refusal naming labels, got %v", tr.Content)
		}
		got, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Description != "old" || got.Labels[rbac.LabelFederatedFrom] != "" {
			t.Errorf("role changed after a refused call: description %q labels %v", got.Spec.Description, got.Labels)
		}
	})

	t.Run("update_jenkins_role_binding", func(t *testing.T) {
		deps := adminDeps()
		store := deps.Store.(*crdstore.Fake)
		crdstore.MustSeed(store, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "b1"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "viewer"}})
		tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{
			"name": "b1", "roleRef": "admin", "labels": labels,
		})
		if !tr.IsError || !strings.Contains(toolText(tr), notAllowedText("labels")) {
			t.Fatalf("want a refusal naming labels, got %v", tr.Content)
		}
		got, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), store, "b1", "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.RoleRef != "viewer" {
			t.Errorf("roleRef = %q after a refused call, want viewer", got.Spec.RoleRef)
		}
	})
}
