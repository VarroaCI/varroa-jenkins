package bundle

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func permanentNode(name string) map[string]any {
	return map[string]any{
		"permanent": map[string]any{
			"name":   name,
			"labels": "varroa-tenant-payments payments-runner",
		},
	}
}

// TestInjectRunnerNodes_Empty asserts ruling (nn): an empty node list still
// writes jenkins.nodes as an empty list, so JCasC drops the runner nodes
// of a tenant that left the cell, and the rest of the document survives.
func TestInjectRunnerNodes_Empty(t *testing.T) {
	const doc = "jenkins:\n  systemMessage: hello\n"
	got, err := InjectRunnerNodes(doc, nil)
	if err != nil {
		t.Fatalf("InjectRunnerNodes: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	jenkins, _ := parsed["jenkins"].(map[string]any)
	nodes, ok := jenkins["nodes"].([]any)
	if !ok || len(nodes) != 0 {
		t.Fatalf("jenkins.nodes = %#v, want an empty list", jenkins["nodes"])
	}
	if jenkins["systemMessage"] != "hello" {
		t.Fatalf("systemMessage = %#v, want hello", jenkins["systemMessage"])
	}
}

// TestInjectRunnerNodes_Create is task 4.3's "create" acceptance:
// jenkins.nodes is created when the document declares none.
func TestInjectRunnerNodes_Create(t *testing.T) {
	const doc = "jenkins:\n  systemMessage: hello\n"
	got, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1")})
	if err != nil {
		t.Fatalf("InjectRunnerNodes: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	jenkins := parsed["jenkins"].(map[string]any)
	nodes, ok := jenkins["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("jenkins.nodes = %v, want one entry", jenkins["nodes"])
	}
	if jenkins["systemMessage"] != "hello" {
		t.Fatalf("systemMessage lost: %v", jenkins["systemMessage"])
	}
}

// TestInjectRunnerNodes_Append is task 4.3's "append" acceptance: an
// existing jenkins.nodes list keeps its entries.
func TestInjectRunnerNodes_Append(t *testing.T) {
	const doc = `jenkins:
  nodes:
    - permanent:
        name: "bundle-declared-node"
`
	got, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1")})
	if err != nil {
		t.Fatalf("InjectRunnerNodes: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	nodes := parsed["jenkins"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("jenkins.nodes = %d entries, want 2", len(nodes))
	}
	names := make([]string, 0, 2)
	for _, n := range nodes {
		name, _ := permanentNodeName(n)
		names = append(names, name)
	}
	if names[0] != "bundle-declared-node" || names[1] != "payments-win-1" {
		t.Fatalf("names = %v, want [bundle-declared-node payments-win-1]", names)
	}
}

// TestInjectRunnerNodes_MultipleRunnersAppendInOrder proves several nodes
// in one call all land in the list, in order.
func TestInjectRunnerNodes_MultipleRunnersAppendInOrder(t *testing.T) {
	const doc = "jenkins: {}\n"
	got, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1"), permanentNode("payments-win-2")})
	if err != nil {
		t.Fatalf("InjectRunnerNodes: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	nodes := parsed["jenkins"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("jenkins.nodes = %d entries, want 2", len(nodes))
	}
}

// TestInjectRunnerNodes_Collision is task 4.3's "collision" acceptance: a
// node name that collides with one the document already declares fails the
// injection.
func TestInjectRunnerNodes_Collision(t *testing.T) {
	const doc = `jenkins:
  nodes:
    - permanent:
        name: "payments-win-1"
`
	_, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1")})
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
	if !strings.Contains(err.Error(), "payments-win-1") {
		t.Errorf("error %q does not name the colliding node", err.Error())
	}
}

// TestInjectRunnerNodes_CollisionWithinNewSet proves two runner nodes
// carrying the same rendered name in one call also collide.
func TestInjectRunnerNodes_CollisionWithinNewSet(t *testing.T) {
	const doc = "jenkins: {}\n"
	_, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1"), permanentNode("payments-win-1")})
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
}

// TestInjectRunnerNodes_NotAList is task 4.3's "a jenkins.nodes value that
// is not a list" acceptance.
func TestInjectRunnerNodes_NotAList(t *testing.T) {
	const doc = `jenkins:
  nodes: "not a list"
`
	_, err := InjectRunnerNodes(doc, []any{permanentNode("payments-win-1")})
	if err == nil {
		t.Fatal("expected an error for a non-list jenkins.nodes, got nil")
	}
}

// TestInjectRunnerNodes_EmptyDocument proves the injector builds a document
// from scratch when jcascYAML is empty.
func TestInjectRunnerNodes_EmptyDocument(t *testing.T) {
	got, err := InjectRunnerNodes("", []any{permanentNode("payments-win-1")})
	if err != nil {
		t.Fatalf("InjectRunnerNodes: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	nodes := parsed["jenkins"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("jenkins.nodes = %d entries, want 1", len(nodes))
	}
}
